package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/config"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// 从模型工具请求到真实 MCP HTTP 服务再回填模型，验证注册表确实接入 Agent Loop。
func TestAgentUsesRegisteredMCP(t *testing.T) {
	mcpServer := server.NewMCPServer("fixture", "1.0")
	mcpCalls := 0
	mcpServer.AddTool(protocol.NewTool("read_text_file", protocol.WithString("path", protocol.Required())), func(ctx context.Context, req protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		mcpCalls++
		return protocol.NewToolResultText("真实 MCP 资料"), nil
	})
	mcpHTTP := httptest.NewServer(server.NewStreamableHTTPServer(mcpServer))
	defer mcpHTTP.Close()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, "mcp.yaml")
	body := fmt.Sprintf("servers:\n  - name: filesystem\n    enabled: true\n    transport: streamable_http\n    url: %q\n    tools: [read_text_file]\n    path_parameters: [path]\n", mcpHTTP.URL+"/mcp")
	if err := os.WriteFile(registry, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "filesystem__read_text_file" {
			t.Errorf("模型未收到唯一 MCP 读取工具: %+v", request.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		if modelCalls == 1 {
			fmt.Fprint(w, `{"id":"one","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"mcp-1","type":"function","function":{"name":"filesystem__read_text_file","arguments":"{\"path\":\"note.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "mcp-1" || !strings.Contains(last.Content, "真实 MCP 资料") {
			t.Errorf("MCP 结果未回填: %+v", last)
		}
		fmt.Fprint(w, `{"id":"two","choices":[{"index":0,"message":{"role":"assistant","content":"根据 note.md 中的 MCP 资料回答。"},"finish_reason":"stop"}]}`)
	}))
	defer model.Close()
	cfg, err := config.NewDefaults[config.Config]()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.APIKey, cfg.Model.Name, cfg.Model.BaseURL = "fixture", "fixture", model.URL+"/v1"
	cfg.MCP.RegistryFile = registry
	a, err := agent.NewWikiAgent(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.RunWithHistory(context.Background(), root, "读取 note.md", nil)
	if err != nil || result == nil || result.Answer == "" || modelCalls != 2 || mcpCalls != 1 {
		t.Fatalf("Agent MCP 闭环失败: %v, model=%d mcp=%d", err, modelCalls, mcpCalls)
	}
}
