package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/lllllan02/wiki-agent/internal/config"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func testConfig() config.MCP { return config.MCP{Timeout: 5 * time.Second, MaxBytes: 1024} }

func findTool(t *testing.T, s *ConnectionSet, name string) tool.InvokableTool {
	t.Helper()
	for _, base := range s.Tools {
		info, err := base.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == name {
			return base.(tool.InvokableTool)
		}
	}
	t.Fatalf("未注册工具 %s", name)
	return nil
}

// 验证真正的 HTTP MCP 握手、认证、发现、过滤和 EINO 工具适配，而不是替身分发函数。
func TestHTTPDiscoveryAndCall(t *testing.T) {
	upstream := server.NewMCPServer("fixture", "1.0")
	upstream.AddTool(protocol.NewTool("read"), func(ctx context.Context, req protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		return protocol.NewToolResultText(strings.Repeat("资料", 1000)), nil
	})
	upstream.AddTool(protocol.NewTool("write"), func(ctx context.Context, req protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		t.Error("白名单外的工具不应执行")
		return protocol.NewToolResultText("bad"), nil
	})
	handler := server.NewStreamableHTTPServer(upstream)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	registry := &Registry{Servers: []Server{{Name: "remote", Enabled: true, Transport: "streamable_http", URL: srv.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer fixture"}, Tools: []string{"read"}}}}
	s, err := registry.Open(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Tools) != 1 {
		t.Fatalf("白名单失效: %d", len(s.Tools))
	}
	result, err := findTool(t, s, "remote__read").InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "资料") {
		t.Fatalf("MCP 调用失败: %s", result)
	}
	registry.Servers[0].Tools = []string{"missing"}
	if _, err := registry.Open(context.Background(), testConfig()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("工具缺失应阻止启动: %v", err)
	}
}

func TestRegistryRejectsInvalidConfiguration(t *testing.T) {
	for _, body := range []string{
		"servers: [{name: one, transport: stdio, tools: []}]",
		"servers: [{name: one, enabled: true, transport: stdio, tools: [read]}]",
		"servers: [{name: one, transport: sse, tools: [read]}]",
		"servers: [{name: one, transport: stdio, tools: [read], typo: true}]",
		"servers: [{name: one, transport: stdio, tools: [read, read]}]",
		"servers: [{name: one, enabled: true, transport: stdio, command: node, args: ['${WIKI_ROOT}'], tools: [read]}]",
		"servers: []\n---\nservers: []",
	} {
		path := filepath.Join(t.TempDir(), "mcp.yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("错误配置被接受: %s", body)
		}
	}
}

// 真实 MCP 集成验收：npm ci --prefix mcp 后显式运行，不让普通单测自动联网安装依赖。
func TestInstalledServers(t *testing.T) {
	if os.Getenv("WIKI_AGENT_MCP_INTEGRATION") != "1" {
		t.Skip("设置 WIKI_AGENT_MCP_INTEGRATION=1 验证已安装的 MCP")
	}
	registry, err := Load("../../../mcp.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Servers) != 7 {
		t.Fatalf("注册项数量错误: %d", len(registry.Servers))
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("# 缓存\n缓存穿透用空值缓存缓解。\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private.txt"), []byte("not markdown"), 0600); err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "note.md"), []byte("second wiki"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Timeout = 20 * time.Second
	s, err := registry.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Tools) != 6 {
		t.Fatalf("预期 6 个只读工具，实际 %d", len(s.Tools))
	}
	for _, tt := range []struct{ name, path, pattern, want string }{
		{"filesystem__list_directory", root, "", "note.md"},
		{"filesystem__read_text_file", filepath.Join(root, "note.md"), "", "缓存穿透"},
		{"filesystem__search_files", root, "**/*.md", "note.md"},
		{"ripgrep__search", root, "缓存穿透", "缓存穿透"},
		{"files__read_file", filepath.Join(root, "note.md"), "", "缓存穿透"},
	} {
		args := map[string]string{"path": tt.path}
		if tt.pattern != "" {
			args["pattern"] = tt.pattern
		}
		payload, _ := json.Marshal(args)
		result, err := findTool(t, s, tt.name).InvokableRun(context.Background(), string(payload))
		if err != nil || !strings.Contains(result, tt.want) {
			t.Fatalf("%s: %s %v", tt.name, result, err)
		}
	}
	secondArgs, _ := json.Marshal(map[string]string{"path": filepath.Join(other, "note.md")})
	result, err := findTool(t, s, "filesystem__read_text_file").InvokableRun(context.Background(), string(secondArgs))
	if err != nil || !strings.Contains(result, "second wiki") || strings.Contains(result, "缓存穿透") {
		t.Fatalf("会话串库: %s %v", result, err)
	}
	// 已安装但默认未启用的服务器只验证发现；不导航外网或写入用户记忆。
	for _, name := range []string{"memory", "playwright", "git"} {
		t.Run(name, func(t *testing.T) {
			for _, entry := range registry.Servers {
				if entry.Name != name {
					continue
				}
				entry.Enabled = true
				if name == "git" {
					if _, err := os.Stat(filepath.Join(registry.baseDir, ".cache/mcp-python/bin/python")); err != nil {
						t.Skip("先安装 mcp/requirements.txt")
					}
					if output, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
						t.Fatalf("初始化临时测试仓库: %s %v", output, err)
					}
				}
				r := &Registry{Servers: []Server{entry}, baseDir: registry.baseDir}
				opened, err := r.Open(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer opened.Close()
				if len(opened.Tools) != len(entry.Tools) {
					t.Fatal("工具发现不完整")
				}
				if name == "memory" {
					if _, err := findTool(t, opened, "memory__search_nodes").InvokableRun(context.Background(), `{"query":"test"}`); err != nil {
						t.Fatal(err)
					}
				}
				if name == "git" {
					gitArgs, _ := json.Marshal(map[string]string{"repo_path": root})
					if _, err := findTool(t, opened, "git__git_status").InvokableRun(context.Background(), string(gitArgs)); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
