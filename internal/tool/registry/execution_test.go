package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

// 一个真实调用必须先经过执行层，越界和参数错误不能到达 MCP。
func TestExecutionBoundary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := &recordingTool{}
	guarded := &executionTool{upstream: upstream, info: &schema.ToolInfo{Name: "filesystem__read_text_file"}, pathParameters: []string{"path"}, timeout: time.Second, maxBytes: 1024}
	for _, args := range []string{`[]`, `{"path":"../other"}`, `{"path":"escape"}`, `{"path":"note.txt"}`} {
		result, err := guarded.InvokableRun(runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: root}), args)
		if err != nil || !strings.Contains(result, `"status":"error"`) {
			t.Fatalf("非法调用被接受: %s", args)
		}
	}
	if upstream.calls != 0 {
		t.Fatal("非法调用到达了 MCP")
	}
	if _, err := guarded.InvokableRun(runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: root}), `{"path":"note.md"}`); err != nil || upstream.calls != 1 || !strings.Contains(upstream.args, filepath.Join(root, "note.md")) {
		t.Fatalf("合法调用未正确转发: %s %v", upstream.args, err)
	}
}

func TestSearchLimitsBeforeDispatch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream := &recordingTool{result: strings.Repeat("资料", 100)}
	guarded := &executionTool{upstream: upstream, info: &schema.ToolInfo{Name: "ripgrep__search"}, pathParameters: []string{"path"}, timeout: time.Second, maxBytes: 21}
	for _, args := range []string{`{"path":".","pattern":"-f/private"}`, `{"path":".","pattern":"ok","maxResults":101}`} {
		result, err := guarded.InvokableRun(runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: root}), args)
		if err != nil || !strings.Contains(result, `"status":"error"`) {
			t.Fatalf("非法搜索被执行: %s", args)
		}
	}
	if upstream.calls != 0 {
		t.Fatal("非法搜索到达了 MCP")
	}
	result, err := guarded.InvokableRun(runcontext.With(context.Background(), runcontext.Metadata{WikiRoot: root}), `{"path":".","pattern":"ok","filePattern":"*.txt","useColors":true}`)
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(upstream.args), &args); err != nil || args["filePattern"] != "*.md" || args["useColors"] != false {
		t.Fatalf("搜索参数未收紧: %s", upstream.args)
	}
	var bounded struct {
		Status    string `json:"status"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result), &bounded); err != nil || !bounded.Truncated || bounded.Status != "truncated" {
		t.Fatalf("过长输出未标注截断: %s", result)
	}
}

type recordingTool struct {
	calls  int
	args   string
	result string
}

func (t *recordingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "upstream"}, nil
}
func (t *recordingTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	t.calls++
	t.args = args
	if t.result != "" {
		return t.result, nil
	}
	return "ok", nil
}
