package middleware

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

type governedTool struct {
	info *schema.ToolInfo
	run  func(context.Context, string, ...tool.Option) (string, error)
}

func (t governedTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t governedTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	return t.run(ctx, args, opts...)
}

func TestChainWrapsResultAndRewritesWikiPath(t *testing.T) {
	root := t.TempDir()
	var received map[string]any
	base := governedTool{
		info: toolInfoWithPath(),
		run: func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
			if err := json.Unmarshal([]byte(args), &received); err != nil {
				t.Fatal(err)
			}
			return "note body", nil
		},
	}
	output, err := invokeGovernedTool(t, root, base, `{"path":"notes/a.md"}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if received["path"] != filepath.Join(root, "notes/a.md") {
		t.Fatalf("path not rewritten into wiki root: %#v", received["path"])
	}
	var contract Contract
	if err := json.Unmarshal([]byte(output), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Status != "ok" || contract.Result != "note body" || contract.Error != "" {
		t.Fatalf("unexpected contract: %+v", contract)
	}
}

func TestChainRejectsBadArgumentsBeforeTool(t *testing.T) {
	called := false
	base := governedTool{
		info: toolInfoWithPath(),
		run: func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
			called = true
			return "", nil
		},
	}
	output, err := invokeGovernedTool(t, t.TempDir(), base, `{"path":123}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("invalid arguments reached tool")
	}
	assertContractError(t, output, "invalid_arguments")
}

func TestSchemaValidationAllowsToolWithoutSchema(t *testing.T) {
	called := false
	base := governedTool{
		info: &schema.ToolInfo{Name: "fixture"},
		run: func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
	}
	middleware := SchemaValidation([]tool.BaseTool{base})
	nodeConfig := compose.ToolsNodeConfig{
		Tools:               []tool.BaseTool{base},
		ToolCallMiddlewares: []compose.ToolMiddleware{middleware},
	}
	node, err := compose.NewToolNode(context.Background(), &nodeConfig)
	if err != nil {
		t.Fatal(err)
	}
	output, err := node.Invoke(context.Background(), schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "fixture", Arguments: `{"unexpected":true}`}}}))
	if err != nil || len(output) != 1 || !called {
		t.Fatalf("tool without schema should pass through: output=%+v err=%v called=%v", output, err, called)
	}
}

func TestChainRejectsPathOutsideWiki(t *testing.T) {
	base := governedTool{
		info: toolInfoWithPath(),
		run: func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
			t.Fatal("outside path reached tool")
			return "", nil
		},
	}
	output, err := invokeGovernedTool(t, t.TempDir(), base, `{"path":"../escape.md"}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertContractError(t, output, "permission_denied")
}

func TestChainConvertsTimeoutToContract(t *testing.T) {
	base := governedTool{
		info: toolInfoWithPath(),
		run: func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	output, err := invokeGovernedTool(t, t.TempDir(), base, `{"path":"notes/a.md"}`, &config.MCP{Timeout: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	assertContractError(t, output, "timeout")
}

func toolInfoWithPath() *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: "fixture",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path": {Type: schema.String, Required: true},
		}),
	}
}

func invokeGovernedTool(t *testing.T, root string, base tool.BaseTool, args string, cfg *config.MCP) (string, error) {
	t.Helper()
	if cfg == nil {
		cfg = &config.MCP{Timeout: time.Second}
	}
	schemaValidation := SchemaValidation([]tool.BaseTool{base})
	middlewares := []compose.ToolMiddleware{
		ContractMiddleware(),
		schemaValidation,
		PathPolicy(map[string]Policy{
			"fixture": {PathParameters: []string{"path"}},
		}),
		Timeout(cfg.Timeout),
	}
	nodeConfig := compose.ToolsNodeConfig{
		Tools:               []tool.BaseTool{base},
		ToolCallMiddlewares: middlewares,
	}
	node, err := compose.NewToolNode(context.Background(), &nodeConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: root})
	output, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "fixture", Arguments: args}}}))
	if err != nil || len(output) != 1 {
		return "", err
	}
	return output[0].Content, nil
}

func assertContractError(t *testing.T, raw, code string) {
	t.Helper()
	var contract Contract
	if err := json.Unmarshal([]byte(raw), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Status != code || contract.Error == "" || contract.Result != "" {
		t.Fatalf("expected %s error contract, got %+v", code, contract)
	}
}
