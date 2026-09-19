package web

import (
	"context"
	"encoding/json"
	"fmt"
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

type fakeWikiAgent struct {
	roots    []string
	sessions []string
}

func (a *fakeWikiAgent) RunWithHistory(ctx context.Context, question string) (*agent.Result, error) {
	metadata := runcontext.From(ctx)
	a.roots = append(a.roots, metadata.WikiRoot)
	a.sessions = append(a.sessions, metadata.SessionID)
	return &agent.Result{Answer: "回答：" + question}, nil
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
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result
	}
	if status, result := call("GET", "/api/project", "window-a", ""); status != 200 || result["root"] != nil {
		t.Fatalf("initial project: %d %+v", status, result)
	}
	if status, _ := call("POST", "/api/chat", "window-a", `{"message":"hello"}`); status != 400 {
		t.Fatal("chat without project accepted")
	}
	if status, result := call("POST", "/api/project", "window-a", fmt.Sprintf(`{"root":%q}`, one)); status != 200 || result["root"] != one {
		t.Fatalf("open: %d %+v", status, result)
	}
	call("POST", "/api/chat", "window-a", `{"message":"first"}`)
	call("POST", "/api/chat", "window-a", `{"message":"second"}`)
	if fake.sessions[0] != "window-a" || fake.sessions[1] != "window-a" || fake.roots[1] != one {
		t.Fatalf("session/roots: %v %v", fake.sessions, fake.roots)
	}
	call("POST", "/api/project", "window-a", fmt.Sprintf(`{"root":%q}`, two))
	call("POST", "/api/chat", "window-a", `{"message":"new wiki"}`)
	if fake.sessions[2] != "window-a" || fake.roots[2] != two {
		t.Fatalf("switch context: %v %v", fake.sessions, fake.roots)
	}
	call("POST", "/api/project", "window-b", fmt.Sprintf(`{"root":%q}`, one))
	call("POST", "/api/chat", "window-b", `{"message":"other window"}`)
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
		{"/api/chat", `{"message":"hello"}`, ""},
		{"/api/chat", `{"message":""}`, "window-a"},
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
}

type blockingWikiAgent struct {
	entered chan struct{}
	release chan struct{}
}

func (a *blockingWikiAgent) RunWithHistory(context.Context, string) (*agent.Result, error) {
	a.entered <- struct{}{}
	<-a.release
	return &agent.Result{Answer: "ok"}, nil
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
			req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":"hello"}`))
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
