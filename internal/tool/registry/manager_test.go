package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/lllllan02/wiki-agent/internal/config"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// 并发创建使用方只建立一组 MCP；单次调用取消不关闭共享连接，应用退出才释放。
func TestSharedConnectionsLifecycle(t *testing.T) {
	upstream := server.NewMCPServer("fixture", "1.0")
	upstream.AddTool(protocol.NewTool("ping"), func(context.Context, protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		return protocol.NewToolResultText("pong"), nil
	})
	handler := server.NewStreamableHTTPServer(upstream)
	var starts atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"initialize"`)) {
			starts.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	registryFile := filepath.Join(t.TempDir(), "mcp.yaml")
	body := fmt.Sprintf("servers:\n  - name: fixture\n    enabled: true\n    transport: streamable_http\n    url: %q\n    tools: [ping]\n", httpServer.URL+"/mcp")
	if err := os.WriteFile(registryFile, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.MCP{RegistryFile: registryFile, Timeout: 5 * time.Second, MaxBytes: 1024}
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	managers := make(chan *Manager, 8)
	var wg sync.WaitGroup
	for i := 0; i < cap(managers); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := LoadMCP(lifetime, cfg)
			if err != nil {
				t.Error(err)
				return
			}
			managers <- m
		}()
	}
	wg.Wait()
	close(managers)
	var first *Manager
	for m := range managers {
		if first == nil {
			first = m
		}
		if m != first {
			t.Fatal("相同配置启动了多个 Manager")
		}
	}
	if first == nil || starts.Load() != 1 {
		t.Fatalf("预期一次初始化，实际 %d", starts.Load())
	}
	defer first.Close()
	tools, err := first.Tools()
	if err != nil {
		t.Fatal(err)
	}
	ping := tools[0].(tool.InvokableTool)
	callCtx, cancelCall := context.WithCancel(context.Background())
	cancelCall()
	if _, err := ping.InvokableRun(callCtx, `{}`); err == nil {
		t.Fatal("已取消调用应失败")
	}
	if result, err := ping.InvokableRun(context.Background(), `{}`); err != nil || !strings.Contains(result, "pong") {
		t.Fatalf("单次取消影响了共享连接: %s %v", result, err)
	}
	cancel()
	select {
	case <-first.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("生命周期取消未关闭连接")
	}
	first.Close() // 幂等
	if _, err := first.Tools(); err == nil {
		t.Fatal("已关闭的工具组仍可获取")
	}
	if _, err := LoadMCP(lifetime, cfg); err == nil {
		t.Fatal("已取消生命周期不应重新创建连接")
	}
	nextLife, stop := context.WithCancel(context.Background())
	defer stop()
	next, err := LoadMCP(nextLife, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next == first || starts.Load() != 2 {
		t.Fatal("新生命周期未创建新连接")
	}
}

func TestFailedInitializationCanRetry(t *testing.T) {
	upstream := server.NewMCPServer("fixture", "1.0")
	upstream.AddTool(protocol.NewTool("ping"), func(context.Context, protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		return protocol.NewToolResultText("pong"), nil
	})
	httpServer := httptest.NewServer(server.NewStreamableHTTPServer(upstream))
	defer httpServer.Close()
	path := filepath.Join(t.TempDir(), "mcp.yaml")
	body := fmt.Sprintf("servers:\n  - name: fixture\n    enabled: true\n    transport: streamable_http\n    url: %q\n    tools: [missing]\n", httpServer.URL+"/mcp")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config.MCP{RegistryFile: path, Timeout: 5 * time.Second, MaxBytes: 1024}
	if _, err := LoadMCP(lifetime, cfg); err == nil {
		t.Fatal("缺失工具应阻止初始化")
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "[missing]", "[ping]")), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := LoadMCP(lifetime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
}
