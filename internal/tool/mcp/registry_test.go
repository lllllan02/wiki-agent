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

func findTool(t *testing.T, s *Session, name string) tool.InvokableTool {
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
	root := t.TempDir()
	s, err := registry.Open(context.Background(), root, testConfig())
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
	var bounded struct {
		Truncated bool   `json:"truncated"`
		Prefix    string `json:"content_prefix"`
	}
	if err := json.Unmarshal([]byte(result), &bounded); err != nil || !bounded.Truncated || len(bounded.Prefix) > testConfig().MaxBytes {
		t.Fatalf("截断契约错误: %s", result)
	}
	registry.Servers[0].Tools = []string{"missing"}
	if _, err := registry.Open(context.Background(), root, testConfig()); err == nil || !strings.Contains(err.Error(), "missing") {
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

func TestPathBoundary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "..", "escape"} {
		if _, err := scopedPath(root, path); err == nil {
			t.Fatalf("越界路径未被拒绝: %s", path)
		}
	}
	if got, err := scopedPath(root, "."); err != nil || got != root {
		t.Fatalf("合法根目录失败: %s %v", got, err)
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
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("# 缓存\n缓存穿透用空值缓存缓解。\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private.txt"), []byte("not markdown"), 0600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "note.md"), []byte("second wiki"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Timeout = 20 * time.Second
	s, err := registry.Open(context.Background(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Tools) != 5 {
		t.Fatalf("预期 5 个只读工具，实际 %d", len(s.Tools))
	}
	for _, tt := range []struct{ name, args, want string }{
		{"filesystem__list_directory", `{"path":"."}`, "note.md"},
		{"filesystem__read_text_file", `{"path":"note.md"}`, "缓存穿透"},
		{"filesystem__search_files", `{"path":".","pattern":"**/*.md"}`, "note.md"},
		{"ripgrep__search", `{"path":".","pattern":"缓存穿透"}`, "缓存穿透"},
	} {
		result, err := findTool(t, s, tt.name).InvokableRun(context.Background(), tt.args)
		if err != nil || !strings.Contains(result, tt.want) {
			t.Fatalf("%s: %s %v", tt.name, result, err)
		}
	}
	read := findTool(t, s, "filesystem__read_text_file")
	for _, path := range []string{filepath.Join(other, "note.md"), "private.txt"} {
		args, _ := json.Marshal(map[string]string{"path": path})
		if _, err := read.InvokableRun(context.Background(), string(args)); err == nil {
			t.Fatalf("不应读取 %s", path)
		}
	}
	if _, err := findTool(t, s, "ripgrep__search").InvokableRun(context.Background(), `{"path":".","pattern":"-f/etc/passwd"}`); err == nil {
		t.Fatal("搜索模式不能注入 rg 选项")
	}
	second, err := registry.Open(context.Background(), other, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	result, err := findTool(t, second, "filesystem__read_text_file").InvokableRun(context.Background(), `{"path":"note.md"}`)
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
				opened, err := r.Open(context.Background(), root, cfg)
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
					if _, err := findTool(t, opened, "git__git_status").InvokableRun(context.Background(), `{"repo_path":"."}`); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
