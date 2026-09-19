// Package web 提供本地网页入口；各会话独立选择 Wiki，运行信息通过 context 传递。
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

//go:embed static/index.html
var staticFiles embed.FS

type WikiAgent interface {
	Stream(context.Context, string, func(string) error) (*agent.Result, error)
}
type Server struct {
	agent         WikiAgent
	mu            sync.Mutex
	conversations map[string]*conversationState
	pickDirectory func(context.Context) (string, error)
}
type conversationState struct {
	mu    sync.Mutex
	root  string
	inbox chan *chatJob
}
type chatJob struct {
	ctx     context.Context
	root    string
	session string
	message string
	onText  func(string) error
	done    chan chatResult
}
type chatResult struct {
	result *agent.Result
	err    error
}

func New(wikiAgent WikiAgent) *Server {
	return &Server{agent: wikiAgent, conversations: make(map[string]*conversationState), pickDirectory: systemDirectoryPicker}
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
	router.POST("/api/chat/stream", s.streamChat)
	return router
}

type projectRequest struct {
	Root string `json:"root"`
}
type projectResponse struct {
	Root  string `json:"root,omitempty"`
	Error string `json:"error,omitempty"`
}
type chatRequest struct {
	Message string `json:"message"`
}
type chatResponse struct {
	Error string `json:"error,omitempty"`
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
	state.mu.Unlock()
	c.JSON(200, projectResponse{Root: root})
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
	c.JSON(200, projectResponse{Root: root})
}

type streamEvent struct {
	Type  string `json:"type"`
	HTML  string `json:"html,omitempty"`
	Error string `json:"error,omitempty"`
}

func (s *Server) streamChat(c *gin.Context) {
	id := conversationID(c)
	if id == "" {
		c.JSON(400, chatResponse{Error: "请提供有效的 X-Conversation-ID"})
		return
	}
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		c.JSON(400, chatResponse{Error: "请输入非空问题"})
		return
	}
	state := s.state(id)
	state.mu.Lock()
	root := state.root
	state.mu.Unlock()
	if root == "" {
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
		session: id,
		message: strings.TrimSpace(req.Message),
		onText:  onText,
		done:    make(chan chatResult, 1),
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
		ctx := runcontext.With(job.ctx, runcontext.Metadata{SessionID: job.session, WikiRoot: job.root})
		result, err := s.agent.Stream(ctx, job.message, job.onText)
		job.done <- chatResult{result: result, err: err}
	}
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
