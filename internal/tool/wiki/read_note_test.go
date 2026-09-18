package wiki

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 同时检查路径与符号链接逃逸，以及中文截断边界；仅测试正常读取无法证明目录限制有效。
func TestReadNoteBoundary(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("缓存穿透"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.md")); err != nil {
		t.Fatal(err)
	}
	output, err := Read(context.Background(), dir, `{"path":"note.md"}`, 7)
	if err != nil {
		t.Fatal(err)
	}
	var note Note
	if err := json.Unmarshal([]byte(output), &note); err != nil {
		t.Fatal(err)
	}
	if note.Content != "缓存" || !note.Truncated || note.Notice == "" || !utf8.ValidString(note.Content) {
		t.Fatalf("bad truncated note: %+v", note)
	}
	for _, args := range []string{
		`{"path":"../secret.md"}`, `{"path":"escape.md"}`, `{"path":"missing.md"}`,
		`{"path":"/etc/passwd"}`, `{"path":"note.txt"}`, `{}`, `null`,
		`{"path":1}`, `{"path":"note.md","extra":true}`, `{"path":"note.md"} {}`,
	} {
		t.Run(args, func(t *testing.T) {
			if _, err := Read(context.Background(), dir, args, 7); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// 刚好达到上限不应标记截断；已取消的读取也不能继续访问文件。
func TestReadNoteExactLimitAndCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := Read(context.Background(), dir, `{"path":"note.md"}`, 3)
	if err != nil || !strings.Contains(output, `"truncated":false`) {
		t.Fatalf("%s %v", output, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, dir, `{"path":"note.md"}`, 3); err == nil {
		t.Fatal("expected cancellation")
	}
}
