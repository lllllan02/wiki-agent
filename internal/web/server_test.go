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

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	"github.com/lllllan02/wiki-agent/internal/store"
)

type checkpointStubAgent struct{}

func (*checkpointStubAgent) Run(context.Context, agent.RunRequest) (*agent.Result, error) {
	return nil, errors.New("unexpected plain stream")
}

func TestPausedStateRequiresSavedCheckpoint(t *testing.T) {
	s := newTestServer(t, &checkpointStubAgent{})
	root, sessionID := t.TempDir(), store.NewSessionID()
	if err := s.sessions.CreateSession(root, sessionID); err != nil {
		t.Fatal(err)
	}
	requestPause := func() {
		t.Helper()
		if err := s.applyImmediateControl(root, sessionID, intentPause); err != nil {
			t.Fatal(err)
		}
	}
	checkpointID := s.sessions.CheckPointID(root, sessionID)
	requestPause()
	if err := s.markStopped(root, sessionID, checkpointID); err == nil {
		t.Fatal("reported a resumable pause without a checkpoint")
	}
	state, err := s.sessions.LoadSessionState(root, sessionID)
	if err != nil || state.RunStatus != store.RunStatusFailed || state.PauseStatus != store.PauseStatusNone {
		t.Fatalf("missing checkpoint state: %+v, %v", state, err)
	}
	requestPause()
	if err := s.sessions.Set(context.Background(), checkpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if err := s.markStopped(root, sessionID, checkpointID); err != nil {
		t.Fatal(err)
	}
	state, err = s.sessions.LoadSessionState(root, sessionID)
	if err != nil || state.RunStatus != store.RunStatusPaused || state.PauseStatus != store.PauseStatusPaused {
		t.Fatalf("saved checkpoint state: %+v, %v", state, err)
	}
}

type progressAgent struct{ release chan struct{} }

func (a *progressAgent) Run(_ context.Context, request agent.RunRequest) (*agent.Result, error) {
	if err := emitTestMessage(request, "# 开始"); err != nil {
		return nil, err
	}
	<-a.release
	if err := emitTestMessage(request, "# 完成"); err != nil {
		return nil, err
	}
	return &agent.Result{Answer: "# 完成"}, nil
}

func newTestServer(t *testing.T, wikiAgent WikiAgent) *Server {
	t.Helper()
	s := New(wikiAgent)
	s.sessions = store.NewIn(t.TempDir())
	return s
}

func emitTestMessage(request agent.RunRequest, text string) error {
	if request.OnEvent == nil {
		return nil
	}
	return request.OnEvent(agent.StreamEvent{Message: schema.AssistantMessage(text, nil)})
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
	srv := httptest.NewServer(newTestServer(t, fake).Handler())
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
	create, _ := http.NewRequest("POST", srv.URL+"/api/sessions", nil)
	create.Header.Set("X-Conversation-ID", "window-a")
	created, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if created.StatusCode != 200 {
		t.Fatalf("create session: %d", created.StatusCode)
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
	if first.Type != "message" || first.Message == nil || first.Message.Content != "# 开始" {
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
	if len(events) != 2 || events[0].Type != "message" || events[0].Message == nil || events[0].Message.Content != "# 完成" || events[1].Type != "done" || events[1].Content != "# 完成" {
		t.Fatalf("final events: %+v", events)
	}
}

func TestPickProject(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, &fakeWikiAgent{})
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

func (markdownAgent) Run(_ context.Context, request agent.RunRequest) (*agent.Result, error) {
	result := &agent.Result{Answer: "# 标题\n\n| A | B |\n| - | - |\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))"}
	return result, emitTestMessage(request, result.Answer)
}

func TestChatRendersSafeMarkdown(t *testing.T) {
	s := newTestServer(t, markdownAgent{})
	handler := s.Handler()
	root := t.TempDir()
	open := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("X-Conversation-ID", "window-a")
	opened := httptest.NewRecorder()
	handler.ServeHTTP(opened, open)
	if opened.Code != 200 {
		t.Fatalf("open: %d", opened.Code)
	}
	create := httptest.NewRequest("POST", "/api/sessions", nil)
	create.Header.Set("X-Conversation-ID", "window-a")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != 200 {
		t.Fatalf("create session: %d", created.Code)
	}
	req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"question"}`))
	req.Header.Set("X-Conversation-ID", "window-a")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, req)
	var response streamEvent
	if err := json.Unmarshal([]byte(strings.SplitN(result.Body.String(), "\n", 2)[0]), &response); err != nil {
		t.Fatal(err)
	}
	if response.Message == nil {
		t.Fatal("stream response did not contain a message")
	}
	html := response.HTML
	if result.Code != 200 || !strings.Contains(html, "<h1>") || !strings.Contains(html, "<table>") {
		t.Fatalf("markdown rendering: %d %s", result.Code, html)
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, `href="javascript:`) {
		t.Fatalf("unsafe markdown: %s", html)
	}
}

type fakeWikiAgent struct {
	roots    []string
	sessions []string
	history  [][]*schema.Message
}

func (a *fakeWikiAgent) Run(ctx context.Context, request agent.RunRequest) (*agent.Result, error) {
	metadata := runcontext.From(ctx)
	a.roots = append(a.roots, metadata.WikiRoot)
	a.sessions = append(a.sessions, metadata.SessionID)
	a.history = append(a.history, append([]*schema.Message(nil), metadata.History...))
	result := &agent.Result{Answer: "回答：" + request.Message}
	return result, emitTestMessage(request, result.Answer)
}
func TestProjectSelectionAndRunContext(t *testing.T) {
	fake := &fakeWikiAgent{}
	one, two := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(newTestServer(t, fake).Handler())
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
	status, result := call("POST", "/api/project", "window-a", fmt.Sprintf(`{"root":%q}`, one))
	if status != 200 || result["root"] != one || result["session_id"] != nil {
		t.Fatalf("open: %d %+v", status, result)
	}
	status, result = call("POST", "/api/sessions", "window-a", "")
	if status != 200 || result["session_id"] == "" {
		t.Fatalf("create session: %d %+v", status, result)
	}
	firstSession := result["session_id"].(string)
	call("POST", "/api/chat/stream", "window-a", `{"message":"first"}`)
	call("POST", "/api/chat/stream", "window-a", `{"message":"second"}`)
	if fake.sessions[0] != firstSession || fake.sessions[1] != firstSession || fake.roots[1] != one {
		t.Fatalf("session/roots: %v %v", fake.sessions, fake.roots)
	}
	status, result = call("POST", "/api/project", "window-a", fmt.Sprintf(`{"root":%q}`, two))
	if status != 200 || result["session_id"] != nil {
		t.Fatalf("switch session: %d %+v", status, result)
	}
	status, result = call("POST", "/api/sessions", "window-a", "")
	if status != 200 || result["session_id"] == "" || result["session_id"] == firstSession {
		t.Fatalf("create switched session: %d %+v", status, result)
	}
	secondSession := result["session_id"].(string)
	call("POST", "/api/chat/stream", "window-a", `{"message":"new wiki"}`)
	if fake.sessions[2] != secondSession || fake.roots[2] != two {
		t.Fatalf("switch context: %v %v", fake.sessions, fake.roots)
	}
	status, result = call("POST", "/api/project", "window-b", fmt.Sprintf(`{"root":%q}`, one))
	if status != 200 || result["session_id"] != nil {
		t.Fatalf("other window session: %d %+v", status, result)
	}
	status, result = call("POST", "/api/sessions", "window-b", "")
	if status != 200 || result["session_id"] == "" || result["session_id"] == firstSession {
		t.Fatalf("create other window session: %d %+v", status, result)
	}
	otherSession := result["session_id"].(string)
	call("POST", "/api/chat/stream", "window-b", `{"message":"other window"}`)
	if fake.sessions[3] != otherSession || fake.roots[3] != one {
		t.Fatalf("window mixed state: %v %v", fake.sessions, fake.roots)
	}
}

func TestChatPersistsMessagesAsJSONL(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, &fakeWikiAgent{})
	handler := s.Handler()
	open := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("Content-Type", "application/json")
	open.Header.Set("X-Conversation-ID", "window-a")
	opened := httptest.NewRecorder()
	handler.ServeHTTP(opened, open)
	if opened.Code != 200 {
		t.Fatalf("open: %d", opened.Code)
	}
	var project projectResponse
	if err := json.Unmarshal(opened.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.NodeID != store.NodeID(root) {
		t.Fatalf("node id: %+v", project)
	}
	create := httptest.NewRequest("POST", "/api/sessions", nil)
	create.Header.Set("X-Conversation-ID", "window-a")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != 200 {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	var createdSession sessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &createdSession); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"persist me"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Conversation-ID", "window-a")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, req)
	if result.Code != 200 {
		t.Fatalf("chat: %d %s", result.Code, result.Body.String())
	}
	messages, err := s.sessions.LoadMessages(root, createdSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Content != "persist me" || messages[1].Role != "assistant" || messages[1].Content != "回答：persist me" {
		t.Fatalf("messages: %+v", messages)
	}
	if _, err := os.Stat(filepath.Join(root, ".wiki-agent")); !os.IsNotExist(err) {
		t.Fatalf("knowledge base was not kept clean: %v", err)
	}
	history := httptest.NewRequest("GET", "/api/chat/messages", nil)
	history.Header.Set("X-Conversation-ID", "window-a")
	historyResult := httptest.NewRecorder()
	handler.ServeHTTP(historyResult, history)
	if historyResult.Code != 200 {
		t.Fatalf("history: %d %s", historyResult.Code, historyResult.Body.String())
	}
	var restored messagesResponse
	if err := json.Unmarshal(historyResult.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 2 || restored.Messages[0].Role != "user" || restored.Messages[0].Content != "persist me" || restored.Messages[1].Role != "assistant" || !strings.Contains(restored.Messages[1].HTML, "回答：persist me") {
		t.Fatalf("restored messages: %+v", restored.Messages)
	}
	second := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"follow up"}`))
	second.Header.Set("Content-Type", "application/json")
	second.Header.Set("X-Conversation-ID", "window-a")
	secondResult := httptest.NewRecorder()
	handler.ServeHTTP(secondResult, second)
	if secondResult.Code != 200 {
		t.Fatalf("second chat: %d %s", secondResult.Code, secondResult.Body.String())
	}
	fake := s.agent.(*fakeWikiAgent)
	if len(fake.history) != 2 || len(fake.history[1]) != 2 || fake.history[1][0].Role != schema.User || fake.history[1][0].Content != "persist me" || fake.history[1][1].Role != schema.Assistant || fake.history[1][1].Content != "回答：persist me" {
		t.Fatalf("agent did not receive stored history: %+v", fake.history)
	}
	list := httptest.NewRequest("GET", "/api/sessions", nil)
	list.Header.Set("X-Conversation-ID", "window-a")
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, list)
	var listedSessions sessionResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &listedSessions); err != nil {
		t.Fatal(err)
	}
	if listed.Code != 200 || len(listedSessions.Sessions) != 1 || listedSessions.Sessions[0].ID != createdSession.SessionID || listedSessions.Sessions[0].MessageCount != 4 {
		t.Fatalf("sessions: %d %+v", listed.Code, listedSessions)
	}
	del := httptest.NewRequest("DELETE", "/api/sessions/"+createdSession.SessionID, nil)
	del.Header.Set("X-Conversation-ID", "window-a")
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, del)
	if deleted.Code != 200 {
		t.Fatalf("delete session: %d %s", deleted.Code, deleted.Body.String())
	}
	history = httptest.NewRequest("GET", "/api/chat/messages", nil)
	history.Header.Set("X-Conversation-ID", "window-a")
	historyResult = httptest.NewRecorder()
	handler.ServeHTTP(historyResult, history)
	if historyResult.Code != 400 {
		t.Fatalf("history after delete: %d", historyResult.Code)
	}
}
func TestProjectAndChatValidation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "note.md")
	if err := os.WriteFile(file, []byte("note"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestServer(t, &fakeWikiAgent{}).Handler())
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
	newTestServer(t, &fakeWikiAgent{}).Handler().ServeHTTP(result, legacy)
	if result.Code != http.StatusNotFound {
		t.Fatalf("legacy chat route still enabled: %d", result.Code)
	}
}

type blockingWikiAgent struct {
	entered chan struct{}
	release chan struct{}
}

func (a *blockingWikiAgent) Run(_ context.Context, request agent.RunRequest) (*agent.Result, error) {
	a.entered <- struct{}{}
	<-a.release
	result := &agent.Result{Answer: "ok"}
	return result, emitTestMessage(request, result.Answer)
}

type cancelAwareAgent struct {
	entered chan struct{}
	stopped chan struct{}
}

type revisionAwareAgent struct {
	entered chan int
}

func (a *revisionAwareAgent) Run(ctx context.Context, request agent.RunRequest) (*agent.Result, error) {
	if request.Message == "first" {
		a.entered <- 1
		<-ctx.Done()
		// Deliberately try a late update: the server must not persist it after
		// a newer intent revision was accepted.
		_ = emitTestMessage(request, "stale output")
		return &agent.Result{Answer: "stale output"}, nil
	}
	a.entered <- 2
	if err := emitTestMessage(request, "fresh output"); err != nil {
		return nil, err
	}
	return &agent.Result{Answer: "fresh output"}, nil
}

func TestAppendInterruptsOldRunAndRejectsLateOutput(t *testing.T) {
	fake := &revisionAwareAgent{entered: make(chan int, 2)}
	s := newTestServer(t, fake)
	handler, root := s.Handler(), t.TempDir()
	open := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("Content-Type", "application/json")
	open.Header.Set("X-Conversation-ID", "one")
	if got := httptest.NewRecorder(); func() int { handler.ServeHTTP(got, open); return got.Code }() != 200 {
		t.Fatal("open failed")
	}
	create := httptest.NewRequest("POST", "/api/sessions", nil)
	create.Header.Set("X-Conversation-ID", "one")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	var session sessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	call := func(message string, done chan<- *httptest.ResponseRecorder) {
		req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(fmt.Sprintf(`{"message":%q}`, message)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-ID", "one")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		done <- recorder
	}
	firstDone, secondDone := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go call("first", firstDone)
	select {
	case got := <-fake.entered:
		if got != 1 {
			t.Fatalf("first run=%d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first did not start")
	}
	go call("add the new constraint", secondDone)
	select {
	case got := <-fake.entered:
		if got != 2 {
			t.Fatalf("second run=%d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second did not start")
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old request did not stop")
	}
	select {
	case second := <-secondDone:
		if !strings.Contains(second.Body.String(), "fresh output") {
			t.Fatalf("new response: %s", second.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new request did not finish")
	}
	messages, err := s.sessions.LoadMessages(root, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "assistant" && message.Content == "stale output" {
			t.Fatal("stale output persisted")
		}
	}
	state, err := s.sessions.LoadSessionState(root, session.SessionID)
	if err != nil || state.IntentRevision != 2 || state.CurrentVisibleAnswer != "fresh output" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func (a *cancelAwareAgent) Run(ctx context.Context, request agent.RunRequest) (*agent.Result, error) {
	a.entered <- struct{}{}
	_ = emitTestMessage(request, "working")
	metadata := runcontext.From(ctx)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(a.stopped)
			return nil, ctx.Err()
		case <-ticker.C:
			paused, err := metadata.PauseRequested()
			if err != nil {
				return nil, err
			}
			if !paused {
				continue
			}
			if err := metadata.CheckpointStore.Set(ctx, metadata.CheckpointID, []byte("checkpoint")); err != nil {
				return nil, err
			}
			close(a.stopped)
			return &agent.Result{Interrupt: &adk.InterruptInfo{}}, nil
		}
	}
}

func TestPauseInterruptsCurrentRunAndPersistsState(t *testing.T) {
	fake := &cancelAwareAgent{entered: make(chan struct{}, 1), stopped: make(chan struct{})}
	s := newTestServer(t, fake)
	handler := s.Handler()
	root := t.TempDir()
	open := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("Content-Type", "application/json")
	open.Header.Set("X-Conversation-ID", "one")
	opened := httptest.NewRecorder()
	handler.ServeHTTP(opened, open)
	if opened.Code != 200 {
		t.Fatalf("open: %d", opened.Code)
	}
	create := httptest.NewRequest("POST", "/api/sessions", nil)
	create.Header.Set("X-Conversation-ID", "one")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != 200 {
		t.Fatalf("create session: %d", created.Code)
	}
	var createdSession sessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &createdSession); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-ID", "one")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
	}()
	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("chat did not start")
	}
	pause := httptest.NewRequest("POST", "/api/chat/pause", nil)
	pause.Header.Set("X-Conversation-ID", "one")
	paused := httptest.NewRecorder()
	handler.ServeHTTP(paused, pause)
	if paused.Code != 200 {
		t.Fatalf("pause: %d %s", paused.Code, paused.Body.String())
	}
	select {
	case <-fake.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not pause")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish after pause")
	}
	state, err := s.sessions.LoadSessionState(root, createdSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RunStatus != store.RunStatusPaused || state.PauseStatus != store.PauseStatusPaused || state.CurrentVisibleAnswer != "working" {
		t.Fatalf("pause state: %+v", state)
	}
	resume := httptest.NewRequest("POST", "/api/chat/resume", nil)
	resume.Header.Set("X-Conversation-ID", "one")
	resumed := httptest.NewRecorder()
	handler.ServeHTTP(resumed, resume)
	if resumed.Code != 200 {
		t.Fatalf("resume: %d %s", resumed.Code, resumed.Body.String())
	}
	state, err = s.sessions.LoadSessionState(root, createdSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RunStatus != store.RunStatusIdle || state.PauseStatus != store.PauseStatusNone {
		t.Fatalf("resume state: %+v", state)
	}
}

func TestDifferentConversationsRunConcurrently(t *testing.T) {
	fake := &blockingWikiAgent{entered: make(chan struct{}, 2), release: make(chan struct{})}
	handler := newTestServer(t, fake).Handler()
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
		create := httptest.NewRequest("POST", "/api/sessions", nil)
		create.Header.Set("X-Conversation-ID", id)
		created := httptest.NewRecorder()
		handler.ServeHTTP(created, create)
		if created.Code != 200 {
			t.Fatalf("create session %s: %d", id, created.Code)
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

func TestProjectCanBeReadWhileConversationRuns(t *testing.T) {
	fake := &blockingWikiAgent{entered: make(chan struct{}, 1), release: make(chan struct{})}
	handler := newTestServer(t, fake).Handler()
	root := t.TempDir()
	open := httptest.NewRequest("POST", "/api/project", strings.NewReader(fmt.Sprintf(`{"root":%q}`, root)))
	open.Header.Set("Content-Type", "application/json")
	open.Header.Set("X-Conversation-ID", "one")
	opened := httptest.NewRecorder()
	handler.ServeHTTP(opened, open)
	if opened.Code != 200 {
		t.Fatalf("open: %d", opened.Code)
	}
	create := httptest.NewRequest("POST", "/api/sessions", nil)
	create.Header.Set("X-Conversation-ID", "one")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != 200 {
		t.Fatalf("create session: %d", created.Code)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("POST", "/api/chat/stream", strings.NewReader(`{"message":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-ID", "one")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != 200 {
			t.Errorf("chat: %d", result.Code)
		}
	}()
	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("chat did not start")
	}
	project := httptest.NewRequest("GET", "/api/project", nil)
	project.Header.Set("X-Conversation-ID", "one")
	read := httptest.NewRecorder()
	handler.ServeHTTP(read, project)
	if read.Code != 200 || !strings.Contains(read.Body.String(), root) {
		t.Fatalf("project during chat: %d %s", read.Code, read.Body.String())
	}
	close(fake.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("chat did not finish")
	}
}
