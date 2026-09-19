package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	agentmiddleware "github.com/lllllan02/wiki-agent/internal/agent/middleware"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	toolmiddleware "github.com/lllllan02/wiki-agent/internal/tool/middleware"
	toolregistry "github.com/lllllan02/wiki-agent/internal/tool/registry"
)

type Result struct {
	Answer    string
	Interrupt *adk.InterruptInfo
}
type StreamEvent struct {
	Message *schema.Message `json:"message"`
}
type EventReporter func(StreamEvent) error

// RunRequest describes one execution. A fresh message and checkpoint resume
// share the same entry point; optional hooks are UI concerns, not new modes.
type RunRequest struct {
	Message string
	OnEvent EventReporter
}

var wikiReadToolNames = []string{
	"filesystem__list_directory",
	"filesystem__search_files",
	"filesystem__read_text_file",
	"filesystem__get_file_info",
	"ripgrep__search",
	"files__read_file",
}

// WikiAgent 只持有可复用的 EINO Agent；目录和会话标识由 context 传入。
type WikiAgent struct {
	agent      adk.Agent
	maxElapsed time.Duration
}

func NewWikiAgent(ctx context.Context, cfg config.Config) (*WikiAgent, error) {
	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.Model.APIKey,
		Model:   cfg.Model.Name,
		BaseURL: cfg.Model.BaseURL,
		Timeout: cfg.Model.Timeout,
	})
	if err != nil {
		return nil, err
	}
	agentConfig := &adk.ChatModelAgentConfig{
		Name:          "wiki_agent",
		Handlers:      []adk.ChatModelAgentMiddleware{&agentmiddleware.Interrupt{}},
		Description:   "检索本地 Markdown 知识库并通过已配置工具收集资料",
		Instruction:   systemPrompt,
		Model:         cm,
		MaxIterations: cfg.Agent.MaxSteps,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				ExecuteSequentially: true,
			},
		},
	}
	manager, err := toolregistry.LoadMCP(ctx, cfg.MCP)
	if err != nil {
		return nil, err
	}
	toolset, err := manager.SelectAvailable(wikiReadToolNames)
	if err != nil {
		return nil, err
	}
	schemaValidation := toolmiddleware.SchemaValidation(toolset.Tools)
	middlewares := []compose.ToolMiddleware{
		toolmiddleware.ContractMiddleware(),
		schemaValidation,
		toolmiddleware.PathPolicy(toolset.Policies),
		toolmiddleware.Timeout(cfg.MCP.Timeout),
	}
	// 注册必须先于 NewChatModelAgent：EINO 的 ReAct 图会根据模型返回的
	// ToolCalls 选择并执行这些工具；WikiAgent 只负责组装业务输入。
	toolregistry.RegisterTools(agentConfig, toolset.Tools, middlewares...)
	agent, err := adk.NewChatModelAgent(ctx, agentConfig)
	if err != nil {
		return nil, err
	}
	return &WikiAgent{agent: agent, maxElapsed: cfg.Agent.MaxElapsed}, nil
}

func (a *WikiAgent) Run(ctx context.Context, request RunRequest) (*Result, error) {
	ctx = runcontext.WithRunID(ctx)
	ctx, cancel := context.WithTimeout(ctx, a.maxElapsed)
	defer cancel()
	metadata := runcontext.From(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (metadata.Resume || metadata.CheckpointID != "" || metadata.CheckpointStore != nil) &&
		(metadata.CheckpointID == "" || metadata.CheckpointStore == nil) {
		return nil, fmt.Errorf("checkpoint id and store are required")
	}
	r := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: a.agent, EnableStreaming: true, CheckPointStore: metadata.CheckpointStore,
	})
	var events *adk.AsyncIterator[*adk.AgentEvent]
	if metadata.Resume {
		var err error
		events, err = r.Resume(ctx, metadata.CheckpointID)
		if err != nil {
			return nil, err
		}
	} else {
		if strings.TrimSpace(request.Message) == "" {
			return nil, fmt.Errorf("用户请求不能为空")
		}
		messages := []*schema.Message{schema.SystemMessage(fmt.Sprintf("本轮 Wiki 根目录：%q", metadata.WikiRoot))}
		messages = append(messages, metadata.History...)
		messages = append(messages, schema.UserMessage(request.Message))
		var options []adk.AgentRunOption
		if metadata.CheckpointID != "" {
			options = append(options, adk.WithCheckPointID(metadata.CheckpointID))
		}
		events = r.Run(ctx, messages, options...)
	}
	return collectEvents(ctx, events, request.OnEvent)
}
