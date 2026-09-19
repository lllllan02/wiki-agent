package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/agent/plugins/checkpoint"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	toolmiddleware "github.com/lllllan02/wiki-agent/internal/tool/middleware"
	toolregistry "github.com/lllllan02/wiki-agent/internal/tool/registry"
)

type Result struct {
	Answer string
}

type CancelRegistrar = checkpoint.CancelRegistrar

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

// Stream 执行一轮 Wiki 请求；消息组装、事件流和单条消息处理都在 WikiAgent 中。
func (a *WikiAgent) Stream(ctx context.Context, request string, onText func(string) error) (*Result, error) {
	return a.stream(ctx, request, nil, onText)
}

func (a *WikiAgent) StreamWithCheckPoint(ctx context.Context, request, checkPointID string, checkPointStore adk.CheckPointStore, onText func(string) error, onCancelReady CancelRegistrar) (*Result, error) {
	plugin := &checkpoint.Plugin{ID: checkPointID, Store: checkPointStore, OnCancelReady: onCancelReady}
	return a.stream(ctx, request, plugin, onText)
}

func (a *WikiAgent) ResumeWithCheckPoint(ctx context.Context, checkPointID string, checkPointStore adk.CheckPointStore, onText func(string) error, onCancelReady CancelRegistrar) (*Result, error) {
	plugin := &checkpoint.Plugin{ID: checkPointID, Store: checkPointStore, Resume: true, OnCancelReady: onCancelReady}
	return a.stream(ctx, "", plugin, onText)
}

func (a *WikiAgent) stream(ctx context.Context, request string, plugin *checkpoint.Plugin, onText func(string) error) (*Result, error) {
	if (plugin == nil || !plugin.Resume) && strings.TrimSpace(request) == "" {
		return nil, fmt.Errorf("用户请求不能为空")
	}
	ctx = runcontext.WithRunID(ctx)
	ctx, cancel := context.WithTimeout(ctx, a.maxElapsed)
	defer cancel()

	config := adk.RunnerConfig{Agent: a.agent, EnableStreaming: true}
	plugin.Register(&config)
	runner := adk.NewRunner(ctx, config)
	var messages []*schema.Message
	if plugin == nil || !plugin.Resume {
		metadata := runcontext.From(ctx)
		messages = []*schema.Message{schema.SystemMessage(fmt.Sprintf("本轮 Wiki 根目录：%q", metadata.WikiRoot))}
		messages = append(messages, metadata.History...)
		messages = append(messages, schema.UserMessage(request))
	}
	iter, err := plugin.Start(ctx, runner, messages)
	if err != nil {
		return nil, err
	}
	var answer string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		event, ok := iter.Next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		current, err := consumeMessage(ctx, event.Output.MessageOutput, onText)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(current) != "" {
			answer = current
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(answer) == "" {
		return nil, fmt.Errorf("model returned an empty final answer")
	}
	return &Result{Answer: answer}, nil
}

// consumeMessage 只向页面发布助手正文；工具消息仍要完整读取。
func consumeMessage(ctx context.Context, output *adk.MessageVariant, onText func(string) error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if output.Role != schema.Assistant {
		_, err := output.GetMessage()
		if err != nil {
			return "", err
		}
		return "", ctx.Err()
	}
	if !output.IsStreaming {
		message, err := output.GetMessage()
		if err != nil || message == nil || message.Content == "" {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return message.Content, onText(message.Content)
	}
	if output.MessageStream == nil {
		return "", fmt.Errorf("assistant message stream is nil")
	}
	defer output.MessageStream.Close()
	var content strings.Builder
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk, err := output.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			return content.String(), nil
		}
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if chunk == nil || chunk.Content == "" {
			continue
		}
		content.WriteString(chunk.Content)
		if err := onText(content.String()); err != nil {
			return "", err
		}
	}
}
