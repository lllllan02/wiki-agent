package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/config"
)

// 用本地 HTTP 服务验证真实 EINO 适配器的请求和消息序列化。
// 响应是固定夹具，因此能稳定定位协议问题，但不能代替真实模型的语义质量验收。
func TestOpenAIAdapterLoop(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect provider request")
		}
		var req struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
				Content    string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.Tools) != 1 {
			t.Error("missing tool schema")
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprint(w, `{"id":"one","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"read-1","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"note.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if len(req.Messages) != 4 || req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID != "read-1" {
			t.Error("incorrect tool result correlation")
		}
		fmt.Fprint(w, `{"id":"two","choices":[{"index":0,"message":{"role":"assistant","content":"根据 note.md：缓存空值可以减少重复查询。"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("缓存空值可以减少重复查询。"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewDefaults[config.Config]()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.APIKey, cfg.Model.Name, cfg.Model.BaseURL = "test-key", "test-model", srv.URL+"/v1"
	cfg.Wiki.MaxReadBytes = 1024
	cfg.Agent.MaxSteps = 3
	wikiAgent, err := agent.NewWikiAgent(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := wikiAgent.RunWithHistory(context.Background(), dir, "读取 note.md", nil)
	if err != nil || result.Answer == "" || calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}
