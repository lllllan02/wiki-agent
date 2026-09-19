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

	"github.com/cloudwego/eino/adk"
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
	Stream(context.Context, string, func(string) error) (*agent.Result, error)
}

type checkPointWikiAgent interface {
	StreamWithCheckPoint(context.Context, string, string, adk.CheckPointStore, func(string) error, agent.CancelRegistrar) (*agent.Result, error)
	ResumeWithCheckPoint(context.Context, string, adk.CheckPointStore, func(string) error, agent.CancelRegistrar) (*agent.Result, error)
}
type Server struct {
	agent         WikiAgent
	mu            sync.Mutex
	conversations map[string]*conversationState
	pickDirectory func(context.Context) (string, error)
	sessions      *store.Store
}
type conversationState struct {
	mu           sync.Mutex
	root         string
	sessionID    string
	inbox        chan *chatJob
	currentPause func() error
	currentAbort context.CancelFunc
	currentRun   int64
}
type chatJob struct {
	ctx     context.Context
	root    string
	session string
	message string
	intent  messageIntent
	onText  func(string) error
	done    chan chatResult
}
type chatResult struct {
	result *agent.Result
	err    error
}

func New(wikiAgent WikiAgent) *Server {
	return &Server{agent: wikiAgent, conversations: make(map[string]*conversationState), pickDirectory: systemDirectoryPicker, sessions: store.New()}
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
	Type  string `json:"type"`
	HTML  string `json:"html,omitempty"`
	Error string `json:"error,omitempty"`
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
	c.Header("Content-Type", "application/x-ndjson; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	// 每个事件占一行 JSON。Flush 让浏览器在本轮 Agent 结束前也能收到 update。
	write := func(event streamEvent) error {
		if err := json.NewEncoder(c.Writer).Encode(event); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}
	// onText 是传给 Agent.Stream 的回调。answer 是当前助手消息截至
	// 这一段的完整 Markdown，而不是单个 token；每次都重绘同一个回答区域。
	onText := func(answer string) error {
		html, err := renderMarkdown(answer)
		if err != nil {
			return err
		}
		return write(streamEvent{Type: "update", HTML: html})
	}
	job := &chatJob{
		ctx:     c.Request.Context(),
		root:    root,
		session: sessionID,
		message: strings.TrimSpace(req.Message),
		intent:  classifyIntent(req.Message),
		onText:  onText,
		done:    make(chan chatResult, 1),
	}
	if req.Resume {
		job.intent = intentResumeRun
	}
	if job.intent == intentPause || job.intent == intentCancel {
		state.mu.Lock()
		pause := state.currentPause
		abort := state.currentAbort
		state.mu.Unlock()
		if err := s.applyImmediateControl(root, sessionID, job.intent); err != nil {
			_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
			return
		}
		if pause != nil {
			if err := pause(); err != nil && !errors.Is(err, adk.ErrCancelTimeout) {
				_ = write(streamEvent{Type: "error", Error: friendlyError(err)})
				return
			}
		} else if abort != nil && (job.intent == intentCancel || s.agentDoesNotCheckpoint()) {
			abort()
		}
	}
	select {
	case state.inbox <- job:
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
	html, err := renderMarkdown(result.Answer)
	if err != nil {
		_ = write(streamEvent{Type: "error", Error: "回答渲染失败"})
		return
	}
	_ = write(streamEvent{Type: "done", HTML: html})
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
	pause := state.currentPause
	abort := state.currentAbort
	state.mu.Unlock()
	if root == "" || sessionID == "" {
		c.JSON(400, controlResponse{Error: "请先打开知识库目录"})
		return
	}
	if intent == intentPause || intent == intentCancel {
		if err := s.applyImmediateControl(root, sessionID, intent); err != nil {
			c.JSON(500, controlResponse{Error: friendlyError(err)})
			return
		}
		if pause != nil {
			if err := pause(); err != nil && !errors.Is(err, adk.ErrCancelTimeout) {
				c.JSON(500, controlResponse{Error: friendlyError(err)})
				return
			}
		} else if abort != nil && (intent == intentCancel || s.agentDoesNotCheckpoint()) {
			abort()
		}
	}
	job := &chatJob{ctx: c.Request.Context(), root: root, session: sessionID, message: intent.String(), intent: intent, done: make(chan chatResult, 1)}
	select {
	case state.inbox <- job:
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
		state = &conversationState{inbox: make(chan *chatJob, 16)}
		s.conversations[id] = state
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
		cpAgent, checkpointID := s.checkPointRunner(job.root, job.session)
		if cpAgent != nil {
			if resume {
				if _, exists, err := s.sessions.Get(job.ctx, checkpointID); err != nil {
					job.done <- chatResult{err: err}
					continue
				} else if !exists {
					job.done <- chatResult{err: fmt.Errorf("未找到可恢复的 checkpoint")}
					continue
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
		}
		if job.intent == intentReplace {
			if err := s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
				sessionState.IntentRevision++
			}); err != nil {
				job.done <- chatResult{err: err}
				continue
			}
		}
		var history []*schema.Message
		if !resume {
			var err error
			history, err = s.sessions.LoadAgentHistory(job.root, job.session)
			if err != nil {
				job.done <- chatResult{err: err}
				continue
			}
			if err := s.sessions.AppendMessage(job.root, store.Message{
				ID:        store.NewMessageID(),
				SessionID: job.session,
				Role:      "user",
				Content:   job.message,
			}); err != nil {
				job.done <- chatResult{err: err}
				continue
			}
		}
		skipCanceled := false
		if err := s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
			sessionState.AcceptedCount = sessionState.ReceivedCount
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
		state.currentPause = nil
		state.currentAbort = abort
		state.mu.Unlock()
		ctx = runcontext.With(ctx, runcontext.Metadata{SessionID: job.session, WikiRoot: job.root, History: history})
		onText := job.onText
		if onText != nil {
			onText = func(answer string) error {
				if err := s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
					sessionState.CurrentVisibleAnswer = answer
				}); err != nil {
					return err
				}
				return job.onText(answer)
			}
		}
		registerCancel := func(pause func() error) {
			state.mu.Lock()
			if state.currentRun == runID {
				state.currentPause = pause
			}
			state.mu.Unlock()
			storedState, err := s.sessions.LoadSessionState(job.root, job.session)
			if err == nil && storedState.PauseStatus == store.PauseStatusRequested {
				go func() { _ = pause() }()
			}
		}
		var result *agent.Result
		var err error
		if cpAgent != nil {
			if resume {
				result, err = cpAgent.ResumeWithCheckPoint(ctx, checkpointID, s.sessions, onText, registerCancel)
			} else {
				result, err = cpAgent.StreamWithCheckPoint(ctx, job.message, checkpointID, s.sessions, onText, registerCancel)
			}
		} else {
			result, err = s.agent.Stream(ctx, job.message, onText)
		}
		state.mu.Lock()
		if state.currentRun == runID {
			state.currentPause = nil
			state.currentAbort = nil
		}
		state.mu.Unlock()
		abort()
		if isPauseError(err) {
			storedState, stateErr := s.sessions.LoadSessionState(job.root, job.session)
			if stateErr != nil {
				job.done <- chatResult{err: stateErr}
				continue
			}
			if storedState.PauseStatus == store.PauseStatusRequested || storedState.RunStatus == store.RunStatusCanceled {
				stopErr := s.markStoppedAfterCancel(job.root, job.session, cpAgent != nil, checkpointID)
				job.done <- chatResult{result: &agent.Result{Answer: "已暂停。"}, err: stopErr}
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
		stateErr := s.sessions.UpdateSessionState(job.root, job.session, func(sessionState *store.SessionState) {
			sessionState.RunStatus = status
			sessionState.PauseStatus = store.PauseStatusNone
			if result != nil {
				sessionState.CurrentVisibleAnswer = result.Answer
			}
		})
		if err == nil {
			err = stateErr
		}
		if err == nil && cpAgent != nil {
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

func (s *Server) checkPointRunner(root, sessionID string) (checkPointWikiAgent, string) {
	cpAgent, ok := s.agent.(checkPointWikiAgent)
	if !ok {
		return nil, ""
	}
	return cpAgent, s.sessions.CheckPointID(root, sessionID)
}

func (s *Server) agentDoesNotCheckpoint() bool {
	_, ok := s.agent.(checkPointWikiAgent)
	return !ok
}

func isPauseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var cancelErr *adk.CancelError
	return errors.As(err, &cancelErr)
}

func (s *Server) applyControl(root, sessionID string, intent messageIntent) error {
	if intent == intentPause {
		if cpAgent, checkpointID := s.checkPointRunner(root, sessionID); cpAgent != nil {
			if _, exists, err := s.sessions.Get(context.Background(), checkpointID); err != nil {
				return err
			} else if !exists {
				return fmt.Errorf("任务未在可恢复的检查点暂停")
			}
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

func (s *Server) markStoppedAfterCancel(root, sessionID string, checkpointEnabled bool, checkpointID string) error {
	state, err := s.sessions.LoadSessionState(root, sessionID)
	if err != nil {
		return err
	}
	resumable := true
	if checkpointEnabled && state.PauseStatus == store.PauseStatusRequested {
		_, resumable, err = s.sessions.Get(context.Background(), checkpointID)
		if err != nil {
			return err
		}
	}
	err = s.sessions.UpdateSessionState(root, sessionID, func(sessionState *store.SessionState) {
		if sessionState.PauseStatus == store.PauseStatusRequested {
			if !resumable {
				sessionState.RunStatus = store.RunStatusFailed
				sessionState.PauseStatus = store.PauseStatusNone
				return
			}
			sessionState.RunStatus = store.RunStatusPaused
			sessionState.PauseStatus = store.PauseStatusPaused
			return
		}
		if sessionState.RunStatus == store.RunStatusCanceled {
			sessionState.PauseStatus = store.PauseStatusNone
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
	if job.onText != nil {
		_ = job.onText(answer)
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
