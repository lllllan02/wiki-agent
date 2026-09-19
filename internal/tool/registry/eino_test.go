package registry

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type testTool struct {
	run func(context.Context, string, ...tool.Option) (string, error)
}

func (testTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "fixture"}, nil
}
func (t testTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	return t.run(ctx, args, opts...)
}

type contextKey struct{}

// 经注册入口和真实 EINO ToolsNode 验证：注册顺序、上下文、调用标识、参数、选项和错误。
func TestNativeMiddlewareChain(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			var events []string
			sentinel := errors.New("upstream failure")
			middleware := func(name string) compose.ToolMiddleware {
				return compose.ToolMiddleware{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
					return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
						events = append(events, name+" before")
						if input.Name != "fixture" || input.CallID != "call-1" || len(input.CallOptions) != 1 {
							t.Fatalf("input: %+v", input)
						}
						output, err := next(ctx, input)
						events = append(events, name+" after")
						return output, err
					}
				}}
			}
			base := testTool{run: func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
				events = append(events, "tool")
				if ctx.Value(contextKey{}) != "request" || args != "raw arguments" || len(opts) != 1 {
					t.Fatal("call changed")
				}
				if fail {
					return "", sentinel
				}
				return "raw result", nil
			}}
			cfg := &adk.ChatModelAgentConfig{}
			cfg.ToolsConfig.Tools = []tool.BaseTool{base}
			cfg.ToolsConfig.ToolCallMiddlewares = []compose.ToolMiddleware{middleware("A")}
			RegisterTools(cfg, nil, middleware("B"))
			if len(cfg.ToolsConfig.Tools) != 0 {
				t.Fatal("static tools should be cleared")
			}
			handler := cfg.Handlers[0].(*fixedTools)
			_, run, err := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
			if err != nil {
				t.Fatal(err)
			}
			config := cfg.ToolsConfig.ToolsNodeConfig
			config.Tools = run.Tools
			node, err := compose.NewToolNode(context.Background(), &config)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), contextKey{}, "request")
			output, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "fixture", Arguments: "raw arguments"}}}), compose.WithToolOption(tool.Option{}))
			if fail {
				if !errors.Is(err, sentinel) {
					t.Fatalf("error not preserved: %v", err)
				}
			} else if err != nil || len(output) != 1 || output[0].Content != "raw result" || output[0].ToolCallID != "call-1" {
				t.Fatalf("output=%v err=%v", output, err)
			}
			if !reflect.DeepEqual(events, []string{"A before", "B before", "tool", "B after", "A after"}) {
				t.Fatal(events)
			}
		})
	}
}

func TestRegistrationPreservesToolsAndHandlers(t *testing.T) {
	first := testTool{}
	second := testTool{}
	existing := &adk.BaseChatModelAgentMiddleware{}
	cfg := &adk.ChatModelAgentConfig{Handlers: []adk.ChatModelAgentMiddleware{existing}}
	cfg.ToolsConfig.Tools = []tool.BaseTool{first}
	added := []tool.BaseTool{second}
	RegisterTools(cfg, added)
	added[0] = nil
	if len(cfg.Handlers) != 2 || cfg.Handlers[0] != existing {
		t.Fatal("existing handlers lost")
	}
	handler := cfg.Handlers[1].(*fixedTools)
	for range 2 {
		_, run, err := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{Tools: []tool.BaseTool{first}})
		if err != nil || len(run.Tools) != 3 || run.Tools[2] == nil {
			t.Fatalf("run=%+v err=%v", run, err)
		}
	}
}
