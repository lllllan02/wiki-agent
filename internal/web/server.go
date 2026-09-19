// Package web 提供本地网页入口；各会话独立选择 Wiki，运行信息通过 context 传递。
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	"github.com/lllllan02/wiki-agent/internal/store"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

//go:embed static/index.html
var staticFiles embed.FS

type WikiAgent interface {
	Run(context.Context, agent.RunRequest) (*agent.Result, error)
}
type Server struct {
	agent         WikiAgent
	mu            sync.Mutex
	conversations map[string]*conversationState
	executions    map[string]*conversationState
	pickDirectory func(context.Context) (string, error)
	sessions      *store.Store
}
type conversationState struct {
	mu           sync.Mutex
	root         string
	sessionID    string
	inbox        chan *chatJob
	currentAbort context.CancelFunc
	currentRun   int64
}
type chatJob struct {
	ctx         context.Context
	root        string
	session     string
	message     string
	receivedSeq int
	intent      messageIntent
	onEvent     agent.EventReporter
	done        chan chatResult
}
type chatResult struct {
	result *agent.Result
	err    error
}

func New(wikiAgent WikiAgent) *Server {
	return &Server{agent: wikiAgent, conversations: make(map[string]*conversationState), executions: make(map[string]*conversationState), pickDirectory: systemDirectoryPicker, sessions: store.New()}
}
func (s *Server) Handler() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	static, _ := fs.Sub(staticFiles, "static")
	index, _ := fs.ReadFile(static, "index.html")
	router.GET("/", func(c *gin.Context) { c.Data(200, "text/html; charset=utf-8", index) })
	router.GET("/api/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	router.GET("/api/project", s.project)
	router.POST("/api/project", s.openProject)
	router.POST("/api/project/pick", s.pickProject)
	router.GET("/api/sessions", s.listSessions)
	router.POST("/api/sessions", s.createSession)
	router.POST("/api/sessions/select", s.selectSession)
	router.DELETE("/api/sessions/:session", s.deleteSession)
	router.GET("/api/chat/messages", s.chatMessages)
	router.POST("/api/chat/stream", s.streamChat)
	router.POST("/api/chat/pause", s.pauseChat)
	router.POST("/api/chat/resume", s.resumeChat)
	router.POST("/api/chat/cancel", s.cancelChat)
	return router
}

type projectRequest struct {
	Root string `json:"root"`
}
type projectResponse struct {
	Root      string `json:"root,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	Error     string `json:"error,omitempty"`
}
type chatRequest struct {
	Message string `json:"message"`
	Resume  bool   `json:"resume,omitempty"`
}
type chatResponse struct {
	Error string `json:"error,omitempty"`
}
type controlResponse struct {
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}
type sessionRequest struct {
	SessionID string `json:"session_id"`
}
type sessionResponse struct {
	SessionID string          `json:"session_id,omitempty"`
	NodeID    string          `json:"node_id,omitempty"`
	Sessions  []store.Session `json:"sessions,omitempty"`
	Error     string          `json:"error,omitempty"`
}
type messageResponse struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	HTML    string `json:"html,omitempty"`
}
type messagesResponse struct {
	Messages []messageResponse `json:"messages,omitempty"`
	Error    string            `json:"error,omitempty"`
}

func (s *Server) project(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, projectResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	root := state.root
	sessionID := state.sessionID
	state.mu.Unlock()
	response := projectResponse{Root: root, SessionID: sessionID}
	if root != "" {
		response.NodeID = store.NodeID(root)
	}
	c.JSON(200, response)
}
func (s *Server) openProject(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, projectResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	var req projectRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Root) == "" {
		c.JSON(400, projectResponse{Error: "请输入要打开的知识库目录"})
		return
	}
	s.setProject(c, id, req.Root)
}
func (s *Server) pickProject(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, projectResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	root, err := s.pickDirectory(c.Request.Context())
	if err != nil {
		if errors.Is(err, errPickerCanceled) {
			c.Status(204)
			return
		}
		c.JSON(500, projectResponse{Error: friendlyError(err)})
		return
	}
	s.setProject(c, id, root)
}
func (s *Server) setProject(c *gin.Context, id, requestedRoot string) {
	requestedRoot = strings.TrimSpace(requestedRoot)
	if requestedRoot == "" {
		c.JSON(400, projectResponse{Error: "请选择存在的 Wiki 目录"})
		return
	}
	root, err := filepath.Abs(requestedRoot)
	if err != nil {
		c.JSON(400, projectResponse{Error: "请选择存在的 Wiki 目录"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	defer state.mu.Unlock()
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		c.JSON(400, projectResponse{Error: "请选择存在的 Wiki 目录"})
		return
	}
	state.root = root
	state.sessionID = ""
	c.JSON(200, projectResponse{Root: root, NodeID: store.NodeID(root)})
}

func (s *Server) listSessions(c *gin.Context) {
	root, ok := s.currentRoot(c)
	if !ok {
		return
	}
	sessions, err := s.sessions.ListSessions(root)
	if err != nil {
		c.JSON(500, sessionResponse{Error: friendlyError(err)})
		return
	}
	c.JSON(200, sessionResponse{NodeID: store.NodeID(root), Sessions: sessions})
}

func (s *Server) createSession(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, sessionResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	root, ok := s.currentRoot(c)
	if !ok {
		return
	}
	sessionID := store.NewSessionID()
	if err := s.sessions.CreateSession(root, sessionID); err != nil {
		c.JSON(500, sessionResponse{Error: friendlyError(err)})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	state.sessionID = sessionID
	state.mu.Unlock()
	c.JSON(200, sessionResponse{SessionID: sessionID, NodeID: store.NodeID(root)})
}

func (s *Server) selectSession(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, sessionResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	root, ok := s.currentRoot(c)
	if !ok {
		return
	}
	var req sessionRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		c.JSON(400, sessionResponse{Error: "请选择有效的对话"})
		return
	}
	sessions, err := s.sessions.ListSessions(root)
	if err != nil {
		c.JSON(500, sessionResponse{Error: friendlyError(err)})
		return
	}
	found := false
	for _, session := range sessions {
		if session.ID == req.SessionID {
			found = true
			break
		}
	}
	if !found {
		c.JSON(404, sessionResponse{Error: "对话不存在"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	state.sessionID = req.SessionID
	state.mu.Unlock()
	c.JSON(200, sessionResponse{SessionID: req.SessionID, NodeID: store.NodeID(root)})
}

func (s *Server) deleteSession(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, sessionResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	root, ok := s.currentRoot(c)
	if !ok {
		return
	}
	sessionID := c.Param("session")
	if err := s.sessions.DeleteSession(root, sessionID); err != nil {
		c.JSON(400, sessionResponse{Error: friendlyError(err)})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	if state.sessionID == sessionID {
		state.sessionID = ""
	}
	state.mu.Unlock()
	c.JSON(200, sessionResponse{NodeID: store.NodeID(root)})
}

func (s *Server) chatMessages(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, messagesResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	root := state.root
	sessionID := state.sessionID
	state.mu.Unlock()
	if root == "" || sessionID == "" {
		c.JSON(400, messagesResponse{Error: "请先打开知识库目录"})
		return
	}
	messages, err := s.sessions.LoadMessages(root, sessionID)
	if err != nil {
		c.JSON(500, messagesResponse{Error: friendlyError(err)})
		return
	}
	response := messagesResponse{Messages: make([]messageResponse, 0, len(messages))}
	for _, message := range messages {
		item := messageResponse{Role: message.Role}
		switch message.Role {
		case "user":
			item.Content = message.Content
		case "assistant":
			html, err := renderMarkdown(message.Content)
			if err != nil {
				c.JSON(500, messagesResponse{Error: "回答渲染失败"})
				return
			}
			item.HTML = html
		default:
			continue
		}
		response.Messages = append(response.Messages, item)
	}
	c.JSON(200, response)
}

func (s *Server) currentRoot(c *gin.Context) (string, bool) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, sessionResponse{Error: "请提供有效的 X-Conversation-ID"})
		return "", false
	}
	state := s.state(id)
	state.mu.Lock()
	root := state.root
	state.mu.Unlock()
	if root == "" {
		c.JSON(400, sessionResponse{Error: "请先打开知识库目录"})
		return "", false
	}
	return root, true
}

type streamEvent struct {
	HTML    string          `json:"html,omitempty"`
	Type    string          `json:"type"`
	Content string          `json:"content,omitempty"`
	Message *schema.Message `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type messageIntent int

const (
	intentAppend messageIntent = iota
	intentPause
	intentResume
	intentResumeRun
	intentCancel
	intentReplace
	intentStatus
)

func (s *Server) streamChat(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, chatResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil || (!req.Resume && strings.TrimSpace(req.Message) == "") {
		c.JSON(400, chatResponse{Error: "请输入非空问题"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	root := state.root
	sessionID := state.sessionID
	state.mu.Unlock()
	if root == "" || sessionID == "" {
		c.JSON(400, chatResponse{Error: "请先打开知识库目录"})
		return
	}
	execution := s.executionState(root, sessionID)
	c.Header("Content-Type", "application/x-ndjson; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	// 每个事件占一行 JSON。Flush 让浏览器在本轮 Agent 结束前也能收到 update。
	write := func(event streamEvent) error {
		text := event.Content
		if event.Message != nil && event.Message.Role == schema.Assistant {
			text = event.Message.Content
		}
		if text != "" {
			var err error
			event.HTML, err = renderMarkdown(text)
			if err != nil {
				return err
			}
		}
		if err := json.NewEncoder(c.Writer).Encode(event); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}
	onEvent := func(event agent.StreamEvent) error {
		return write(streamEvent{Type: "message", Message: event.Message})
	}
	intent := classifyIntent(req.Message)
	if req.Resume {
		intent = intentResumeRun
	}
	if intent == intentStatus {
		state, err := s.sessions.LoadSessionState(root, sessionID)
		if err != nil {
			_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
			return
		}
		answer := "当前状态：" + string(state.RunStatus) + "，暂停状态：" + string(state.PauseStatus)
		_ = write(streamEvent{Type: "done", Content: answer})
		return
	}
	job := &chatJob{
		ctx:     c.Request.Context(),
		root:    root,
		session: sessionID,
		message: strings.TrimSpace(req.Message),
		intent:  intent,
		onEvent: onEvent,
		done:    make(chan chatResult, 1),
	}
	if !req.Resume && (job.intent == intentAppend || job.intent == intentReplace) {
		received, err := s.sessions.ReceiveUserMessage(root, store.Message{ID: store.NewMessageID(), SessionID: sessionID, Role: "user", Content: job.message})
		if err != nil {
			_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
			return
		}
		job.receivedSeq = received.ReceivedSeq
		// The incoming instruction is accepted before cancelling the old run,
		// so every old callback immediately becomes revision-stale.
		if _, err := s.sessions.AcceptUserMessage(root, sessionID, received.ReceivedSeq, job.intent == intentReplace); err != nil {
			_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
			return
		}
		execution.mu.Lock()
		abort := execution.currentAbort
		execution.mu.Unlock()
		if abort != nil {
			abort()
		}
	}
	if job.intent == intentPause || job.intent == intentCancel {
		execution.mu.Lock()
		abort := execution.currentAbort
		execution.mu.Unlock()
		if err := s.applyImmediateControl(root, sessionID, job.intent); err != nil {
			_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
			return
		}
		if job.intent == intentCancel && abort != nil {
			abort()
		}
	}
	select {
	case execution.inbox <- job:
	case <-c.Request.Context().Done():
		return
	}
	outcome := <-job.done
	result, err := outcome.result, outcome.err
	if err != nil {
		if c.Request.Context().Err() == nil {
			_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
		}
		return
	}
	// 只有 EINO 事件迭代结束、Agent 返回最终 Result 后，才发送 done。
	_ = write(streamEvent{Type: "done", Content: result.Answer})
}

func (s *Server) pauseChat(c *gin.Context) {
	s.controlChat(c, intentPause)
}

func (s *Server) resumeChat(c *gin.Context) {
	s.controlChat(c, intentResume)
}

func (s *Server) cancelChat(c *gin.Context) {
	s.controlChat(c, intentCancel)
}

func (s *Server) controlChat(c *gin.Context, intent messageIntent) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, controlResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	root := state.root
	sessionID := state.sessionID
	state.mu.Unlock()
	if root == "" || sessionID == "" {
		c.JSON(400, controlResponse{Error: "请先打开知识库目录"})
		return
	}
	execution := s.executionState(root, sessionID)
	execution.mu.Lock()
	abort := execution.currentAbort
	execution.mu.Unlock()
	if intent == intentPause || intent == intentCancel {
		if err := s.applyImmediateControl(root, sessionID, intent); err != nil {
			c.JSON(500, controlResponse{Error: friendlyError(err)})
			return
		}
		if intent == intentCancel && abort != nil {
			abort()
		}
	}
	job := &chatJob{ctx: c.Request.Context(), root: root, session: sessionID, message: intent.String(), intent: intent, done: make(chan chatResult, 1)}
	select {
	case execution.inbox <- job:
	case <-c.Request.Context().Done():
		return
	}
	outcome := <-job.done
	if outcome.err != nil {
		c.JSON(500, controlResponse{Error: friendlyError(outcome.err)})
		return
	}
	c.JSON(200, controlResponse{Status: intent.String()})
}

func renderMarkdown(answer string) (string, error) {
	var rendered bytes.Buffer
	err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(answer), &rendered)
	return rendered.String(), err
}

var errPickerCanceled = errors.New("directory picker canceled")

func systemDirectoryPicker(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("当前系统暂不支持目录选择弹窗")
	}
	cmd := exec.CommandContext(ctx, "osascript", "-e", `POSIX path of (choose folder with prompt "选择知识库目录")`)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), "-128") {
			return "", errPickerCanceled
		}
		return "", errors.New("无法打开系统目录选择窗口")
	}
	return strings.TrimSpace(string(output)), nil
}
func (s *Server) state(id string) *conversationState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.conversations[id]
	if state == nil {
		state = &conversationState{}
		s.conversations[id] = state
	}
	return state
}

// executionState is keyed by the durable session identity, not the browser
// window. Selecting one session in two windows must still produce one loop.
func (s *Server) executionState(root, sessionID string) *conversationState {
	key := store.NodeID(root) + ":" + sessionID
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.executions[key]
	if state == nil {
		state = &conversationState{root: root, sessionID: sessionID, inbox: make(chan *chatJob, 16)}
		s.executions[key] = state
		go s.runConversation(state)
	}
	return state
}
func (s *Server) runConversation(state *conversationState) {
	for job := range state.inbox {
		switch job.intent {
		case intentPause, intentResume, intentCancel:
			err := s.applyControl(job.root, job.session, job.intent)
			job.done <- chatResult{result: &agent.Result{Answer: controlAnswer(job.intent)}, err: err}
			continue
		case intentStatus:
			s.answerStatus(job)
			continue
		}
		resume := job.intent == intentResumeRun
		checkpointID := s.sessions.CheckPointID(job.root, job.session)
		sessionState, err := s.sessions.LoadSessionState(job.root, job.session)
		if err != nil {
			job.done <- chatResult{err: err}
			continue
		}
		if resume {
			if _, exists, err := s.sessions.Get(job.ctx, checkpointID); err != nil {
				job.done <- chatResult{err: err}
				continue
			} else if !exists {
				job.done <- chatResult{err: fmt.Errorf("未找到可恢复的 checkpoint")}
				continue
			}
			if sessionState.CheckpointRevision != sessionState.IntentRevision || sessionState.CheckpointAccepted != sessionState.AcceptedThroughSeq {
				_ = s.sessions.Delete(context.Background(), checkpointID)
				// The opaque checkpoint contains the old model context. Rebuild a
				// fresh run from the durable accepted messages instead of resuming it.
				messages, loadErr := s.sessions.LoadMessages(job.root, job.session)
				if loadErr != nil {
					job.done <- chatResult{err: loadErr}
					continue
				}
				for i := len(messages) - 1; i >= 0; i-- {
					if messages[i].Role == "user" && messages[i].ReceivedSeq == sessionState.AcceptedThroughSeq {
						job.message, job.receivedSeq, resume = messages[i].Content, messages[i].ReceivedSeq, false
						break
					}
				}
				if resume {
					job.done <- chatResult{err: fmt.Errorf("找不到可重建的已接纳任务")}
					continue
				}
			}
		} else {
			state, err := s.sessions.LoadSessionState(job.root, job.session)
			if err != nil {
				job.done <- chatResult{err: err}
				continue
			}
			if state.RunStatus == store.RunStatusPaused || state.PauseStatus == store.PauseStatusRequested {
				job.done <- chatResult{err: fmt.Errorf("任务已暂停，请先继续或取消")}
				continue
			}
			if err := s.sessions.Delete(job.ctx, checkpointID); err != nil {
				job.done <- chatResult{err: err}
				continue
			}
		}
		var history []*schema.Message
		if !resume {
			if job.receivedSeq == 0 {
				job.done <- chatResult{err: fmt.Errorf("缺少已接收的用户消息")}
				continue
			}
			history, err = s.sessions.LoadExecutionHistory(job.root, job.session, job.receivedSeq, sessionState.EffectiveFromSeq)
			if err != nil {
				job.done <- chatResult{err: err}
				continue
			}
		}
		skipCanceled := false
		if err := s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
			sessionState.AcceptedCount = sessionState.AcceptedThroughSeq
			if sessionState.RunStatus == store.RunStatusCanceled {
				skipCanceled = true
				return
			}
			sessionState.RunStatus = store.RunStatusRunning
			if sessionState.PauseStatus != store.PauseStatusRequested {
				sessionState.PauseStatus = store.PauseStatusNone
			}
			sessionState.CurrentVisibleAnswer = ""
		}); err != nil {
			job.done <- chatResult{err: err}
			continue
		}
		if skipCanceled {
			job.done <- chatResult{result: &agent.Result{Answer: "已取消。"}}
			continue
		}
		ctx, abort := context.WithCancel(job.ctx)
		state.mu.Lock()
		state.currentRun++
		runID := state.currentRun
		state.currentAbort = abort
		state.mu.Unlock()
		revision := sessionState.IntentRevision
		if err := s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
			sessionState.ActiveRunID = runID
			sessionState.ActiveIntentRevision = revision
		}); err != nil {
			abort()
			job.done <- chatResult{err: err}
			continue
		}
		onEvent := job.onEvent
		if onEvent != nil {
			onEvent = func(event agent.StreamEvent) error {
				if event.Message != nil && event.Message.Content != "" {
					current, err := s.sessions.UpdateVisibleAnswerIfCurrent(job.root, job.session, runID, revision, event.Message.Content)
					if err != nil {
						return err
					}
					if !current {
						return context.Canceled
					}
				}
				return job.onEvent(event)
			}
		}
		ctx = runcontext.With(ctx, runcontext.Metadata{
			SessionID:       job.session,
			WikiRoot:        job.root,
			History:         history,
			Resume:          resume,
			CheckpointID:    checkpointID,
			CheckpointStore: s.sessions,
			PauseRequested: func() (bool, error) {
				state, err := s.sessions.LoadSessionState(job.root, job.session)
				return state.PauseStatus == store.PauseStatusRequested, err
			},
		})
		var result *agent.Result
		err = nil
		runRequest := agent.RunRequest{
			Message: job.message,
			OnEvent: onEvent,
		}
		result, err = s.agent.Run(ctx, runRequest)
		state.mu.Lock()
		if state.currentRun == runID {
			state.currentAbort = nil
		}
		state.mu.Unlock()
		abort()
		storedState, stateErr := s.sessions.LoadSessionState(job.root, job.session)
		if stateErr != nil {
			job.done <- chatResult{err: stateErr}
			continue
		}
		// An append/replace accepted while this call was running invalidates all
		// of its output, including a late successful result.
		if storedState.IntentRevision != revision {
			_ = s.sessions.Delete(context.Background(), checkpointID)
			_ = s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
				if sessionState.ActiveRunID == runID {
					sessionState.ActiveRunID = 0
					sessionState.ActiveIntentRevision = 0
					sessionState.RunStatus = store.RunStatusIdle
				}
			})
			job.done <- chatResult{err: fmt.Errorf("任务已根据新要求重新规划")}
			continue
		}
		// Native tool/agent interrupts are successful suspensions, not empty
		// answers. Persist the same paused state as a user-requested pause.
		interrupted := err == nil && result != nil && result.Interrupt != nil
		if interrupted && storedState.RunStatus != store.RunStatusCanceled {
			if err := s.applyImmediateControl(job.root, job.session, intentPause); err != nil {
				job.done <- chatResult{err: err}
				continue
			}
		}
		if interrupted || (errors.Is(err, context.Canceled) && storedState.RunStatus == store.RunStatusCanceled) {
			storedState, stateErr := s.sessions.LoadSessionState(job.root, job.session)
			if stateErr != nil {
				job.done <- chatResult{err: stateErr}
				continue
			}
			if storedState.PauseStatus == store.PauseStatusRequested || storedState.RunStatus == store.RunStatusCanceled {
				stopErr := s.markStopped(job.root, job.session, checkpointID)
				answer := "已暂停。"
				if storedState.RunStatus == store.RunStatusCanceled {
					answer = "已取消。"
				}
				job.done <- chatResult{result: &agent.Result{Answer: answer}, err: stopErr}
				continue
			}
		}
		if err == nil && result != nil {
			err = s.sessions.AppendMessage(job.root, store.Message{
				ID:        store.NewMessageID(),
				SessionID: job.session,
				Role:      "assistant",
				Content:   result.Answer,
			})
		}
		status := store.RunStatusIdle
		if err != nil {
			status = store.RunStatusFailed
		}
		stateErr = s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
			sessionState.RunStatus = status
			sessionState.PauseStatus = store.PauseStatusNone
			if sessionState.ActiveRunID == runID {
				sessionState.ActiveRunID = 0
				sessionState.ActiveIntentRevision = 0
			}
			if result != nil {
				sessionState.CurrentVisibleAnswer = result.Answer
			}
		})
		if err == nil {
			err = stateErr
		}
		if err == nil {
			err = s.sessions.Delete(context.Background(), checkpointID)
		}
		job.done <- chatResult{result: result, err: err}
	}
}

func (s *Server) applyImmediateControl(root, sessionID string, intent messageIntent) error {
	return s.sessions.UpdateSessionState(root, sessionID, func(sessionState *store.SessionState) {
		switch intent {
		case intentPause:
			sessionState.PauseStatus = store.PauseStatusRequested
		case intentCancel:
			sessionState.RunStatus = store.RunStatusCanceled
			sessionState.PauseStatus = store.PauseStatusNone
		}
	})
}

func (s *Server) applyControl(root, sessionID string, intent messageIntent) error {
	if intent == intentCancel {
		if err := s.sessions.Delete(context.Background(), s.sessions.CheckPointID(root, sessionID)); err != nil {
			return err
		}
	}
	if intent == intentPause {
		checkpointID := s.sessions.CheckPointID(root, sessionID)
		if _, exists, err := s.sessions.Get(context.Background(), checkpointID); err != nil {
			return err
		} else if !exists {
			return fmt.Errorf("任务未在可恢复的检查点暂停")
		}
	}
	return s.sessions.UpdateSessionState(root, sessionID, func(sessionState *store.SessionState) {
		switch intent {
		case intentPause:
			sessionState.RunStatus = store.RunStatusPaused
			sessionState.PauseStatus = store.PauseStatusPaused
		case intentResume:
			if sessionState.RunStatus == store.RunStatusPaused || sessionState.PauseStatus != store.PauseStatusNone {
				sessionState.RunStatus = store.RunStatusIdle
			}
			sessionState.PauseStatus = store.PauseStatusNone
		case intentCancel:
			sessionState.RunStatus = store.RunStatusCanceled
			sessionState.PauseStatus = store.PauseStatusNone
		}
	})
}

func (s *Server) markStopped(root, sessionID string, checkpointID string) error {
	state, err := s.sessions.LoadSessionState(root, sessionID)
	if err != nil {
		return err
	}
	resumable := true
	if state.PauseStatus == store.PauseStatusRequested {
		_, resumable, err = s.sessions.Get(context.Background(), checkpointID)
		if err != nil {
			return err
		}
	}
	err = s.sessions.UpdateSessionState(root, sessionID, func(sessionState *store.SessionState) {
		sessionState.ActiveRunID = 0
		sessionState.ActiveIntentRevision = 0
		if sessionState.PauseStatus == store.PauseStatusRequested {
			if !resumable {
				sessionState.RunStatus = store.RunStatusFailed
				sessionState.PauseStatus = store.PauseStatusNone
				return
			}
			sessionState.RunStatus = store.RunStatusPaused
			sessionState.PauseStatus = store.PauseStatusPaused
			sessionState.CheckpointRevision = sessionState.IntentRevision
			sessionState.CheckpointAccepted = sessionState.AcceptedThroughSeq
			return
		}
		if sessionState.RunStatus == store.RunStatusCanceled {
			sessionState.PauseStatus = store.PauseStatusNone
			sessionState.ActiveRunID = 0
			sessionState.ActiveIntentRevision = 0
			return
		}
		sessionState.RunStatus = store.RunStatusIdle
	})
	if err != nil {
		return err
	}
	if !resumable {
		return fmt.Errorf("暂停时未保存 checkpoint，无法从断点继续")
	}
	return nil
}

func (s *Server) answerStatus(job *chatJob) {
	state, err := s.sessions.LoadSessionState(job.root, job.session)
	if err != nil {
		job.done <- chatResult{err: err}
		return
	}
	answer := "当前状态：" + string(state.RunStatus) + "，暂停状态：" + string(state.PauseStatus)
	if job.onEvent != nil {
		_ = job.onEvent(agent.StreamEvent{Message: schema.AssistantMessage(answer, nil)})
	}
	job.done <- chatResult{result: &agent.Result{Answer: answer}}
}
func conversationID(c *gin.Context) string {
	id := c.GetHeader("X-Conversation-ID")
	if len(id) == 0 || len(id) > 128 {
		return ""
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return ""
		}
	}
	return id
}
func friendlyError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 800 {
		message = message[:800] + "…"
	}
	return message
}

func classifyIntent(message string) messageIntent {
	text := strings.TrimSpace(message)
	if text == "" {
		return intentAppend
	}
	if strings.Contains(text, "状态") || strings.Contains(text, "进度") || strings.Contains(text, "到哪") {
		return intentStatus
	}
	if strings.Contains(text, "取消") || strings.Contains(text, "停止任务") || strings.Contains(text, "不用做了") {
		return intentCancel
	}
	if strings.Contains(text, "暂停") || strings.Contains(text, "先停") || strings.Contains(text, "等一下") {
		return intentPause
	}
	if strings.Contains(text, "继续") || strings.Contains(text, "接着") || strings.Contains(text, "恢复") {
		return intentResume
	}
	if strings.Contains(text, "换个任务") || strings.Contains(text, "改成") || strings.Contains(text, "不要原来") || strings.Contains(text, "放弃原") {
		return intentReplace
	}
	return intentAppend
}

func (intent messageIntent) String() string {
	switch intent {
	case intentPause:
		return "paused"
	case intentResume:
		return "resumed"
	case intentCancel:
		return "canceled"
	case intentReplace:
		return "replaced"
	case intentStatus:
		return "status"
	default:
		return "appended"
	}
}

func controlAnswer(intent messageIntent) string {
	switch intent {
	case intentPause:
		return "已暂停。"
	case intentResume:
		return "已继续。"
	case intentCancel:
		return "已取消。"
	default:
		return "已接收。"
	}
}
