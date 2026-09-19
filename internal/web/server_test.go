package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

type progressAgent struct{ release chan struct{} }

func (a *progressAgent) Stream(_ context.Context, _ string, onText func(string) error) (*agent.Result, error) {
	if err := onText("# 开始"); err != nil {
		return nil, err
	}
	<-a.release
	if err := onText("# 完成"); err != nil {
		return nil, err
	}
	return &agent.Result{Answer: "# 完成"}, nil
}

func TestStreamChatFlushesBeforeCompletion(t *testing.T) {
	fake := &progressAgent{release: make(chan struct{})}
	defer func() {
		select {
		case <-fake.release:
		default:
			close(fake.release)
		}
	}()
	srv := httptest.NewServer(New(fake).Handler())
	defer srv.Close()
	root := t.TempDir()
	open, _ := http.NewRequest("POST", srv.URL+"/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("X-Conversation-ID", "window-a")
	opened, err := http.DefaultClient.Do(open)
	if err != nil {
		t.Fatal(err)
	}
	opened.Body.Close()
	if opened.StatusCode != 200 {
		t.Fatalf("open: %d", opened.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/chat/stream", strings.NewReader(`{"message":"question"}`))
	req.Header.Set("X-Conversation-ID", "window-a")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("stream response: %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	if !scanner.Scan() {
		t.Fatalf("first update missing: %v", scanner.Err())
	}
	var first streamEvent
	if err := json.Unmarshal(scanner.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Type != "update" || !strings.Contains(first.HTML, "<h1>开始</h1>") {
		t.Fatalf("first update: %+v", first)
	}
	close(fake.release)
	var events []streamEvent
	for scanner.Scan() {
		var event streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "update" || events[1].Type != "done" || !strings.Contains(events[1].HTML, "<h1>完成</h1>") {
		t.Fatalf("final events: %+v", events)
	}
}

func TestPickProject(t *testing.T) {
	root := t.TempDir()
	s := New(&fakeWikiAgent{})
	s.pickDirectory = func(context.Context) (string, error) { return root, nil }
	handler := s.Handler()
	request := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/project/pick", nil)
		req.Header.Set("X-Conversation-ID", id)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if got := request(""); got.Code != 400 {
		t.Fatalf("missing conversation id: %d", got.Code)
	}
	if got := request("window-a"); got.Code != 200 || !strings.Contains(got.Body.String(), root) {
		t.Fatalf("pick: %d %s", got.Code, got.Body.String())
	}
	s.pickDirectory = func(context.Context) (string, error) { return "", errPickerCanceled }
	if got := request("window-a"); got.Code != 204 {
		t.Fatalf("cancel: %d", got.Code)
	}
	s.pickDirectory = func(context.Context) (string, error) { return "", errors.New("picker failed") }
	if got := request("window-a"); got.Code != 500 {
		t.Fatalf("picker error: %d", got.Code)
	}
	s.pickDirectory = func(context.Context) (string, error) { return filepath.Join(root, "missing"), nil }
	if got := request("window-a"); got.Code != 400 {
		t.Fatalf("invalid directory: %d", got.Code)
	}
}

type markdownAgent struct{}

func (markdownAgent) Stream(_ context.Context, _ string, onText func(string) error) (*agent.Result, error) {
	result := &agent.Result{Answer: "# 标题\n\n| A | B |\n| - | - |\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))"}
	return result, onText(result.Answer)
}

func TestChatRendersSafeMarkdown(t *testing.T) {
	s := New(markdownAgent{})
	handler := s.Handler()
	root := t.TempDir()
	open := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("X-Conversation-ID", "window-a")
	opened := httptest.NewRecorder()
	handler.ServeHTTP(opened, open)
	if opened.Code != 200 {
		t.Fatalf("open: %d", opened.Code)
	}
	req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"question"}`))
	req.Header.Set("X-Conversation-ID", "window-a")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, req)
	var response streamEvent
	if err := json.Unmarshal([]byte(strings.SplitN(result.Body.String(), "\n", 2)[0]), &response); err != nil {
		t.Fatal(err)
	}
	if result.Code != 200 || !strings.Contains(response.HTML, "<h1>") || !strings.Contains(response.HTML, "<table>") {
		t.Fatalf("markdown rendering: %d %s", result.Code, response.HTML)
	}
	if strings.Contains(response.HTML, "<script>") || strings.Contains(response.HTML, `href="javascript:`) {
		t.Fatalf("unsafe markdown: %s", response.HTML)
	}
}

type fakeWikiAgent struct {
	roots    []string
	sessions []string
}

func (a *fakeWikiAgent) Stream(ctx context.Context, question string, onText func(string) error) (*agent.Result, error) {
	metadata := runcontext.From(ctx)
	a.roots = append(a.roots, metadata.WikiRoot)
	a.sessions = append(a.sessions, metadata.SessionID)
	result := &agent.Result{Answer: "回答：" + question}
	return result, onText(result.Answer)
}
func TestProjectSelectionAndRunContext(t *testing.T) {
	fake := &fakeWikiAgent{}
	one, two := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(New(fake).Handler())
	defer srv.Close()
	call := func(method, path, id, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if id != "" {
			req.Header.Set("X-Conversation-ID", id)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		var result map[string]any
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if path == "/api/chat/stream" && resp.StatusCode == 200 {
			for {
				var event map[string]any
				err := decoder.Decode(&event)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				result = event
			}
			if result["type"] != "done" {
				t.Fatalf("stream did not finish: %+v", result)
			}
		}
		return resp.StatusCode, result
	}
	if status, result := call("GET", "/api/project", "window-a", ""); status != 200 || result["root"] != nil {
		t.Fatalf("initial project: %d %+v", status, result)
	}
	if status, _ := call("POST", "/api/chat/stream", "window-a", `{"message":"hello"}`); status != 400 {
		t.Fatal("chat without project accepted")
	}
	if status, result := call("POST", "/api/project", "window-a", fmt.Sprintf(`{"root":%q}`, one)); status != 200 || result["root"] != one {
		t.Fatalf("open: %d %+v", status, result)
	}
	call("POST", "/api/chat/stream", "window-a", `{"message":"first"}`)
	call("POST", "/api/chat/stream", "window-a", `{"message":"second"}`)
	if fake.sessions[0] != "window-a" || fake.sessions[1] != "window-a" || fake.roots[1] != one {
		t.Fatalf("session/roots: %v %v", fake.sessions, fake.roots)
	}
	call("POST", "/api/project", "window-a", fmt.Sprintf(`{"root":%q}`, two))
	call("POST", "/api/chat/stream", "window-a", `{"message":"new wiki"}`)
	if fake.sessions[2] != "window-a" || fake.roots[2] != two {
		t.Fatalf("switch context: %v %v", fake.sessions, fake.roots)
	}
	call("POST", "/api/project", "window-b", fmt.Sprintf(`{"root":%q}`, one))
	call("POST", "/api/chat/stream", "window-b", `{"message":"other window"}`)
	if fake.sessions[3] != "window-b" || fake.roots[3] != one {
		t.Fatalf("window mixed state: %v %v", fake.sessions, fake.roots)
	}
}
func TestProjectAndChatValidation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "note.md")
	if err := os.WriteFile(file, []byte("note"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(&fakeWikiAgent{}).Handler())
	defer srv.Close()
	for _, item := range []struct{ path, body, id string }{
		{"/api/project", `{"root":"/wiki"}`, ""},
		{"/api/project", `{"root":""}`, "window-a"},
		{"/api/project", fmt.Sprintf(`{"root":%q}`, filepath.Join(root, "missing")), "window-a"},
		{"/api/project", fmt.Sprintf(`{"root":%q}`, file), "window-a"},
		{"/api/chat/stream", `{"message":"hello"}`, ""},
		{"/api/chat/stream", `{"message":""}`, "window-a"},
	} {
		req, _ := http.NewRequest("POST", srv.URL+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/json")
		if item.id != "" {
			req.Header.Set("X-Conversation-ID", item.id)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("accepted %s %s: %d", item.path, item.body, resp.StatusCode)
		}
	}
	legacy := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":"hello"}`))
	legacy.Header.Set("X-Conversation-ID", "window-a")
	result := httptest.NewRecorder()
	New(&fakeWikiAgent{}).Handler().ServeHTTP(result, legacy)
	if result.Code != http.StatusNotFound {
		t.Fatalf("legacy chat route still enabled: %d", result.Code)
	}
}

type blockingWikiAgent struct {
	entered chan struct{}
	release chan struct{}
}

func (a *blockingWikiAgent) Stream(_ context.Context, _ string, onText func(string) error) (*agent.Result, error) {
	a.entered <- struct{}{}
	<-a.release
	result := &agent.Result{Answer: "ok"}
	return result, onText(result.Answer)
}
func TestDifferentConversationsRunConcurrently(t *testing.T) {
	fake := &blockingWikiAgent{entered: make(chan struct{}, 2), release: make(chan struct{})}
	handler := New(fake).Handler()
	root := t.TempDir()
	for _, id := range []string{"one", "two"} {
		req := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-ID", id)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != 200 {
			t.Fatalf("open %s: %d", id, result.Code)
		}
	}
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Conversation-ID", id)
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, req)
			if result.Code != 200 {
				t.Errorf("chat %s: %d", id, result.Code)
			}
		}(id)
	}
	defer func() { close(fake.release); wg.Wait() }()
	for i := 0; i < 2; i++ {
		select {
		case <-fake.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("different conversations serialized")
		}
	}
}
