// Package web 提供 Gin 网页入口。它只负责 HTTP、会话和展示，具体执行交给 WikiAgent。
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"io/fs"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"github.com/lllllan02/wiki-agent/internal/agent"
)

//go:embed static/index.html
var staticFiles embed.FS

type WikiAgent interface {
	RunWithHistory(context.Context, string, string, []*schema.Message) (*agent.Result, error)
}

type Server struct {
	agent    WikiAgent
	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	projectRoot string
	history     []*schema.Message
}

func New(wikiAgent WikiAgent) *Server {
	return &Server{agent: wikiAgent, sessions: make(map[string]*sessionState)}
}

// Handler 使用 Gin 统一处理路由、JSON 响应和 Recovery；页面文件保持在 static 目录。
func (s *Server) Handler() *gin.Engine {
	// 页面是本地应用，不需要 Gin 的开发路由日志；错误仍由 Recovery 返回并记录。
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	static, _ := fs.Sub(staticFiles, "static")
	index, _ := fs.ReadFile(static, "index.html")
	router.GET("/", func(c *gin.Context) { c.Data(200, "text/html; charset=utf-8", index) })
	router.GET("/api/health", s.health)
	router.GET("/api/project", s.project)
	router.POST("/api/project", s.openProject)
	router.POST("/api/chat", s.chat)
	return router
}

func (s *Server) health(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) }

type chatRequest struct {
	Message string `json:"message"`
}
type projectRequest struct {
	Root string `json:"root"`
}
type projectResponse struct {
	Root  string `json:"root,omitempty"`
	Error string `json:"error,omitempty"`
}
type chatResponse struct {
	Answer string `json:"answer,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) project(c *gin.Context) {
	sessionID := sessionCookie(c)
	s.mu.Lock()
	state := s.state(sessionID)
	root := state.projectRoot
	s.mu.Unlock()
	c.JSON(200, projectResponse{Root: root})
}

func (s *Server) openProject(c *gin.Context) {
	var req projectRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Root) == "" {
		c.JSON(400, projectResponse{Error: "请输入要打开的知识库目录"})
		return
	}
	root := strings.TrimSpace(req.Root)
	sessionID := sessionCookie(c)
	s.mu.Lock()
	state := s.state(sessionID)
	// “打开项目”只表示当前浏览器会话选择了一个知识库根目录。
	// 真实读取发生在 read_note 工具中，那里会再次校验目录和文件边界；
	// 这里不预先创建 Reader 或 Agent 状态，避免引入没有后续用途的生命周期。
	// 切换目录后清空历史，避免旧目录的工具结果混入新项目。
	state.projectRoot = root
	state.history = nil
	s.mu.Unlock()
	c.JSON(200, projectResponse{Root: root})
}

func (s *Server) chat(c *gin.Context) {
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		c.JSON(400, chatResponse{Error: "请输入非空问题"})
		return
	}
	sessionID := sessionCookie(c)
	// 同一个会话串行执行，避免两次请求同时推进同一份消息历史。
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(sessionID)
	if strings.TrimSpace(state.projectRoot) == "" {
		c.JSON(400, chatResponse{Error: "请先在页面顶部打开一个知识库目录"})
		return
	}
	result, err := s.agent.RunWithHistory(c.Request.Context(), state.projectRoot, strings.TrimSpace(req.Message), state.history)
	if err != nil {
		c.JSON(502, chatResponse{Error: friendlyError(err)})
		return
	}
	state.history = result.Messages
	c.JSON(200, chatResponse{Answer: result.Answer})
}

func (s *Server) state(sessionID string) *sessionState {
	state := s.sessions[sessionID]
	if state == nil {
		state = &sessionState{}
		s.sessions[sessionID] = state
	}
	return state
}

func sessionCookie(c *gin.Context) string {
	if id, err := c.Cookie("wiki_session"); err == nil && id != "" {
		return id
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		b = []byte("local-session")
	}
	id := hex.EncodeToString(b)
	c.SetCookie("wiki_session", id, 86400*30, "/", "", false, true)
	return id
}

func friendlyError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 800 {
		message = message[:800] + "…"
	}
	return message
}
