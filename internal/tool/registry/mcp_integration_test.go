package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

func TestInstalledSharedGit(t *testing.T) {
	if os.Getenv("WIKI_AGENT_MCP_INTEGRATION") != "1" {
		t.Skip("设置 WIKI_AGENT_MCP_INTEGRATION=1 验证已安装的 Git MCP")
	}
	python, err := filepath.Abs("../../../.cache/mcp-python/bin/python")
	if err != nil {
		t.Fatal(err)
	}
	registryFile := filepath.Join(t.TempDir(), "mcp.yaml")
	body := fmt.Sprintf("servers:\n  - name: git\n    enabled: true\n    transport: stdio\n    command: %q\n    args: [-m, mcp_server_git]\n    tools: [git_status]\n    path_parameters: [repo_path]\n", python)
	if err := os.WriteFile(registryFile, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, err := LoadMCP(ctx, config.MCP{RegistryFile: registryFile, Timeout: 20 * time.Second, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	roots := []string{t.TempDir(), t.TempDir()}
	for i, root := range roots {
		if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
			t.Fatalf("git init: %s %v", out, err)
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("wiki-%d.md", i)), []byte("note"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := manager.Tools()
	if err != nil {
		t.Fatal(err)
	}
	read := tools[0].(tool.InvokableTool)
	for i, root := range roots {
		ctx := runcontext.With(ctx, runcontext.Metadata{WikiRoot: root})
		result, err := read.InvokableRun(ctx, `{"repo_path":"."}`)
		if err != nil || !strings.Contains(result, fmt.Sprintf("wiki-%d.md", i)) || strings.Contains(result, fmt.Sprintf("wiki-%d.md", 1-i)) {
			t.Fatalf("Git 目录未隔离: %s %v", result, err)
		}
		outside, _ := json.Marshal(map[string]string{"repo_path": roots[1-i]})
		if _, err := read.InvokableRun(ctx, string(outside)); err == nil {
			t.Fatal("Git 越界路径未拒绝")
		}
	}
}

// 显式验收已安装的 Filesystem Server：同一个进程跨 Wiki 使用，执行层仍拦截越界读取。
func TestInstalledSharedFilesystem(t *testing.T) {
	if os.Getenv("WIKI_AGENT_MCP_INTEGRATION") != "1" {
		t.Skip("设置 WIKI_AGENT_MCP_INTEGRATION=1 验证已安装的 MCP")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, err := LoadMCP(lifetime, config.MCP{RegistryFile: "../../../mcp.yaml", Timeout: 20 * time.Second, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(root, "note.md"), []byte(root), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := manager.Tools()
	if err != nil {
		t.Fatal(err)
	}
	connections := manager.connections
	second, err := manager.Tools()
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != second[0] {
		t.Fatal("工具对象未复用")
	}
	ctx := runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: b})
	if manager.connections != connections {
		t.Fatal("默认 MCP 不应按目录增加进程")
	}
	var read tool.InvokableTool
	for _, base := range second {
		info, err := base.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "filesystem__read_text_file" {
			read = base.(tool.InvokableTool)
		}
	}
	if read == nil {
		t.Fatal("缺少 Filesystem 读取工具")
	}
	result, err := read.InvokableRun(ctx, `{"path":"note.md"}`)
	if err != nil || !strings.Contains(result, b) {
		t.Fatalf("第二个目录无法使用共享 Filesystem: %s %v", result, err)
	}
	outside, _ := json.Marshal(map[string]string{"path": filepath.Join(a, "note.md")})
	if _, err := read.InvokableRun(ctx, string(outside)); err == nil {
		t.Fatal("共享进程不得绕过当前 Wiki 的访问边界")
	}
}
