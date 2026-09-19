// Package web 提供本地网页入口；各会话独立选择 Wiki，运行信息通过 context 传递。
package web

import (
	"context"
	"embed"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

//go:embed static/index.html
var staticFiles embed.FS

type WikiAgent interface {
	RunWithHistory(context.Context, string) (*agent.Result, error)
}
type Server struct {
	agent         WikiAgent
	mu            sync.Mutex
	conversations map[string]*conversationState
}
type conversationState struct {
	mu   sync.Mutex
	root string
}

func New(wikiAgent WikiAgent) *Server {
	return &Server{agent: wikiAgent, conversations: make(map[string]*conversationState)}
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
	router.POST("/api/chat", s.chat)
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
	Answer string `json:"answer,omitempty"`
	Error  string `json:"error,omitempty"`
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
	root := strings.TrimSpace(req.Root)
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
func (s *Server) chat(c *gin.Context) {
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
	// 同一对话串行推进，其他对话可以并行使用同一或不同 Wiki 的工具。
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.root == "" {
		c.JSON(400, chatResponse{Error: "请先打开知识库目录"})
		return
	}
	ctx := runcontext.With(c.Request.Context(), runcontext.Metadata{SessionID: id, WikiRoot: state.root})
	result, err := s.agent.RunWithHistory(ctx, strings.TrimSpace(req.Message))
	if err != nil {
		c.JSON(502, chatResponse{Error: friendlyError(err)})
		return
	}
	c.JSON(200, chatResponse{Answer: result.Answer})
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
