package agent

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/config"
	mcptools "github.com/lllllan02/wiki-agent/internal/tool/mcp"
	"github.com/lllllan02/wiki-agent/internal/tool/wiki"
)

type Result struct {
	Messages []*schema.Message
	Answer   string
}

// WikiAgent 是 Web 层复用的 Agent 对象。
//
// 模型客户端和 EINO ChatModelAgent 在创建 WikiAgent 时完成初始化。
// RunWithHistory 为本轮知识库建立 MCP 会话，并交给新的 EINO Runner，避免把一次运行
// 的状态误放进长期复用对象里。模型复用；启用 MCP 时 Agent 使用本轮工具集合。
type WikiAgent struct {
	cfg      config.Config
	agent    adk.Agent
	model    *openai.ChatModel
	registry *mcptools.Registry
}

func NewWikiAgent(ctx context.Context, cfg config.Config) (*WikiAgent, error) {
	baseURL, err := normalizeBaseURL(cfg.Model.BaseURL)
	if err != nil {
		return nil, err
	}
	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.Model.APIKey,
		Model:   cfg.Model.Name,
		BaseURL: baseURL,
		Timeout: cfg.Model.Timeout,
	})
	if err != nil {
		return nil, err
	}
	registry, err := mcptools.Load(cfg.MCP.RegistryFile)
	if err != nil {
		return nil, err
	}
	a, err := newChatAgent(ctx, cfg, cm, []tool.BaseTool{wiki.ReadNoteTool{MaxBytes: cfg.Wiki.MaxReadBytes}})
	if err != nil {
		return nil, err
	}
	return &WikiAgent{cfg: cfg, agent: a, model: cm, registry: registry}, nil
}

func newChatAgent(ctx context.Context, cfg config.Config, cm *openai.ChatModel, tools []tool.BaseTool) (adk.Agent, error) {
	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "wiki_agent",
		Description:   "检索本地 Markdown 知识库并通过已配置 MCP 收集资料",
		Instruction:   systemPrompt,
		Model:         cm,
		MaxIterations: cfg.Agent.MaxSteps,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               tools,
				ExecuteSequentially: true,
			},
		},
	})
}

func (a *WikiAgent) RunWithHistory(ctx context.Context, root string, request string, history []*schema.Message) (*Result, error) {
	if strings.TrimSpace(request) == "" {
		return &Result{Messages: append([]*schema.Message(nil), history...)}, fmt.Errorf("用户请求不能为空")
	}
	session, err := a.registry.Open(ctx, root, a.cfg.MCP)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	runAgent := a.agent
	if len(session.Tools) > 0 {
		tools := append([]tool.BaseTool(nil), session.Tools...)
		// 现成读取工具启用后不再向模型暴露同功能的本地 read_note。
		hasRead := false
		for _, base := range tools {
			info, err := base.Info(ctx)
			if err != nil {
				return nil, err
			}
			if info.Name == "filesystem__read_text_file" {
				hasRead = true
			}
		}
		if !hasRead {
			tools = append(tools, wiki.ReadNoteTool{MaxBytes: a.cfg.Wiki.MaxReadBytes})
		}
		runAgent, err = newChatAgent(ctx, a.cfg, a.model, tools)
		if err != nil {
			return nil, err
		}
	}
	messagesForRun := append(append([]*schema.Message(nil), history...), schema.UserMessage(request))
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: runAgent})
	iter := runner.Run(ctx, messagesForRun, adk.WithToolOptions([]tool.Option{wiki.WithProject(root, a.cfg.Wiki.MaxReadBytes)}))
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
	messages := append(append([]*schema.Message(nil), history...), schema.UserMessage(request), schema.AssistantMessage(answer, nil))
	return &Result{Messages: messages, Answer: answer}, nil
}

// normalizeBaseURL 只为明确支持的 DeepSeek OpenAI 兼容地址补上 /v1。
//
// EINO 的 OpenAI 适配器会自己追加 /chat/completions；如果用户把 DeepSeek
// 根域名直接写成 https://api.deepseek.com，最终请求容易落到错误地址并返回 HTML。
// 这个处理属于 WikiAgent 创建模型前的配置清洗，没有必要单独拆成一层 model 包。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("model.base_url 必须是完整 URL（例如 https://api.deepseek.com/v1）")
	}
	if strings.EqualFold(u.Hostname(), "api.deepseek.com") && strings.Trim(u.Path, "/") == "" {
		u.Path = "/v1"
	}
	return strings.TrimRight(u.String(), "/"), nil
}
