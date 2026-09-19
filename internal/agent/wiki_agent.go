package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/config"
	toolregistry "github.com/lllllan02/wiki-agent/internal/tool/registry"
)

type Result struct {
	Answer string
}

// WikiAgent 只持有可复用的 EINO Agent；目录和会话标识由 context 传入。
type WikiAgent struct {
	agent adk.Agent
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
	tools, err := manager.Tools()
	if err != nil {
		return nil, err
	}
	// 当前工具都来自 MCP。将来新增自定义 Go Tool 时，在这里合并后一起注册。
	// 注册必须先于 NewChatModelAgent；每轮 Run 只使用这些已注册的工具。
	toolregistry.RegisterTools(agentConfig, tools)
	agent, err := adk.NewChatModelAgent(ctx, agentConfig)
	if err != nil {
		return nil, err
	}
	return &WikiAgent{agent: agent}, nil
}

func (a *WikiAgent) RunWithHistory(ctx context.Context, request string) (*Result, error) {
	if strings.TrimSpace(request) == "" {
		return nil, fmt.Errorf("用户请求不能为空")
	}
	// 尚未接入会话存储，当前只使用本次请求；后续按 context 中的 SessionID 加载历史。
	messagesForRun := []*schema.Message{schema.UserMessage(request)}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: a.agent})
	iter := runner.Run(ctx, messagesForRun)
	var answer string
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, err := event.Output.MessageOutput.GetMessage()
		if err != nil {
			return nil, err
		}
		if msg != nil && msg.Role == schema.Assistant && strings.TrimSpace(msg.Content) != "" {
			answer = msg.Content
		}
	}
	if strings.TrimSpace(answer) == "" {
		return nil, fmt.Errorf("model returned an empty final answer")
	}
	return &Result{Answer: answer}, nil
}
