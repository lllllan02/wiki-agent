package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// 模型依次调用两个真实注册的 MCP 工具，验证名称分发和工具调用 ID 回填。
func TestAgentSearchReadAnswerWithMCP(t *testing.T) {
	mcpServer := server.NewMCPServer("fixture", "1.0")
	searchCalls, readCalls := 0, 0
	mcpServer.AddTool(protocol.NewTool("search", protocol.WithString("pattern", protocol.Required()), protocol.WithString("path", protocol.Required())), func(ctx context.Context, req protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		searchCalls++
		return protocol.NewToolResultText("notes/cache.md: 缓存穿透"), nil
	})
	mcpServer.AddTool(protocol.NewTool("read_text_file", protocol.WithString("path", protocol.Required())), func(ctx context.Context, req protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		readCalls++
		return protocol.NewToolResultText("缓存穿透可以缓存空值"), nil
	})
	mcpHandler := server.NewStreamableHTTPServer(mcpServer)
	initializations := 0
	mcpHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"initialize"`)) {
			initializations++
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer mcpHTTP.Close()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "cache.md"), []byte("缓存穿透可以缓存空值"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, "mcp.yaml")
	body := fmt.Sprintf("servers:\n  - name: ripgrep\n    enabled: true\n    transport: streamable_http\n    url: %q\n    tools: [search]\n    path_parameters: [path]\n  - name: filesystem\n    enabled: true\n    transport: streamable_http\n    url: %q\n    tools: [read_text_file]\n    path_parameters: [path]\n", mcpHTTP.URL+"/mcp", mcpHTTP.URL+"/mcp")
	if err := os.WriteFile(registry, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	var receivedRoots []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
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
		if !request.Stream {
			t.Error("model request did not enable streaming")
		}
		names := map[string]bool{}
		for _, item := range request.Tools {
			names[item.Function.Name] = true
		}
		if len(names) != 2 || !names["ripgrep__search"] || !names["filesystem__read_text_file"] {
			t.Errorf("unexpected tool registry: %+v", names)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		var payload string
		switch (modelCalls-1)%3 + 1 {
		case 1:
			if len(request.Messages) != 3 || request.Messages[0].Role != "system" || request.Messages[1].Role != "system" || request.Messages[2].Role != "user" {
				t.Errorf("单轮请求混入其他轮消息: %+v", request.Messages)
			}
			if len(request.Messages) == 3 {
				receivedRoots = append(receivedRoots, request.Messages[1].Content)
			}
			payload = `{"id":"one","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"search-1","type":"function","function":{"name":"ripgrep__search","arguments":"{\"pattern\":\"缓存穿透\",\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`
		case 2:
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "search-1" || !strings.Contains(last.Content, "notes/cache.md") {
				t.Errorf("search result not correlated: %+v", last)
			}
			payload = `{"id":"two","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"read-2","type":"function","function":{"name":"filesystem__read_text_file","arguments":"{\"path\":\"notes/cache.md\"}"}}]},"finish_reason":"tool_calls"}]}`
		default:
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "read-2" || !strings.Contains(last.Content, "缓存空值") {
				t.Errorf("read result not correlated: %+v", last)
			}
			payload = `{"id":"three","choices":[{"index":0,"delta":{"role":"assistant","content":"根据 notes/cache.md，可缓存空值。"},"finish_reason":"stop"}]}`
		}
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	defer model.Close()
	cfg, err := config.NewDefaults[config.Config]()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.APIKey, cfg.Model.Name, cfg.Model.BaseURL = "fixture", "fixture", model.URL+"/v1"
	cfg.MCP.RegistryFile = registry
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := agent.NewWikiAgent(lifetime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "notes", "cache.md"), []byte("note"), 0600); err != nil {
		t.Fatal(err)
	}
	// 两个独立构造的 Agent 在 A → B → A 切换目录时共用同一组 MCP。
	secondAgent, err := agent.NewWikiAgent(lifetime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	agents := []*agent.WikiAgent{a, secondAgent, a}
	for i, runRoot := range []string{root, other, root} {
		result, err := agents[i].Stream(runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: runRoot, SessionID: "fixture"}), "缓存穿透如何处理？", func(string) error { return nil })
		if err != nil || result == nil || result.Answer == "" {
			t.Fatalf("Agent MCP 闭环失败: %v", err)
		}
	}
	for i, root := range []string{root, other, root} {
		if len(receivedRoots) != 3 || receivedRoots[i] != fmt.Sprintf("本轮 Wiki 根目录：%q", root) {
			t.Fatalf("模型未收到本轮目录: %v", receivedRoots)
		}
	}
	if modelCalls != 9 || searchCalls != 3 || readCalls != 3 || initializations != 2 {
		t.Fatalf("MCP 未跨运行复用: model=%d search=%d read=%d initialize=%d", modelCalls, searchCalls, readCalls, initializations)
	}
}
