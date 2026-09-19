package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// 注册必须先于 NewChatModelAgent：EINO 的 ReAct 图会根据模型返回的
	// ToolCalls 选择并执行这些工具；下面的 Stream/consumeMessage 不负责调度工具。
	toolregistry.RegisterTools(agentConfig, tools)
	agent, err := adk.NewChatModelAgent(ctx, agentConfig)
	if err != nil {
		return nil, err
	}
	return &WikiAgent{agent: agent}, nil
}

// Stream 从 EINO Runner 读取整轮 Agent 事件。onText 是调用方传进来的 Go 回调，
// 不是 EINO API：每收到一段助手正文，就以「当前这条助手消息的完整正文」调用它。
// Web 层传入的实现会把正文转成 Markdown HTML，写入 HTTP 响应并 Flush 给浏览器。
// 工具调用的判断和执行发生在 EINO 的 ReAct 图中，见 docs/streaming.md。
func (a *WikiAgent) Stream(ctx context.Context, request string, onText func(string) error) (*Result, error) {
	if strings.TrimSpace(request) == "" {
		return nil, fmt.Errorf("用户请求不能为空")
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: a.agent, EnableStreaming: true})
	// Query 启动 EINO 的「模型 → 如需工具则执行工具 → 再次调用模型」循环。
	// Next 取的是 Agent 事件；一个事件里还可能有需要逐段 Recv 的 MessageStream。
	iter := runner.Query(ctx, request)
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
		current, err := consumeMessage(event.Output.MessageOutput, onText)
		if err != nil {
			return nil, err
		}
		// 新一轮模型输出会成为最新回答；工具调用前可能出现的临时文字不会
		// 与工具调用后的最终回答拼接。Web 每次也会替换同一个回答区域。
		if strings.TrimSpace(current) != "" {
			answer = current
		}
	}
	if strings.TrimSpace(answer) == "" {
		return nil, fmt.Errorf("model returned an empty final answer")
	}
	return &Result{Answer: answer}, nil
}

// 一个 EINO 消息事件既可能是完整消息，也可能带有需要主动读取并关闭的消息流。
// ToolCalls 是助手消息上的结构化字段，由 EINO 内部检查；这里只发布 Content。
func consumeMessage(output *adk.MessageVariant, onText func(string) error) (string, error) {
	if output.Role != schema.Assistant {
		// 工具结果可能是流；读完它，但不把工具原始输出显示为助手回答。
		_, err := output.GetMessage()
		return "", err
	}
	if !output.IsStreaming {
		message, err := output.GetMessage()
		if err != nil || message == nil || message.Content == "" {
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
		// Recv 的 EOF 只表示这条消息结束；外层 Next 才表示整轮 Agent 结束。
		chunk, err := output.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			return content.String(), nil
		}
		if err != nil {
			return "", err
		}
		if chunk == nil || chunk.Content == "" {
			// 例如工具调用参数可能在 ToolCalls 中，Content 为空时无需推给页面。
			continue
		}
		content.WriteString(chunk.Content)
		// 这是同步回调：Web 写出这一版内容后，才继续读取下一段模型输出。
		if err := onText(content.String()); err != nil {
			return "", err
		}
	}
}
