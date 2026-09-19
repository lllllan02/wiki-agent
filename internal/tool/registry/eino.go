package registry

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

// RegisterTools 将任意来源的标准 Go 工具注册到 Agent，包括自定义 Tool 和 MCP Tool。
// 同一组工具对象可以被多个 Agent 复用；工具实现本身要能安全处理并发调用。
// EINO v0.9.19 的静态 ReAct 配置会在并发 Run 时改写 cancelCtx；
// 将工具保存在 handler 中，每轮独立构建执行配置，避免 SDK 数据竞争。
// 必须在 NewChatModelAgent 之前调用；升级 EINO 后可重新评估这层适配。
func RegisterTools(cfg *adk.ChatModelAgentConfig, tools []tool.BaseTool) {
	// 把配置里原有的工具与传入的工具合并，不根据来源分别注册。
	// 只复制切片，不重新创建工具；之后每轮 Run 都使用这些同一对象。
	all := append(append([]tool.BaseTool(nil), cfg.ToolsConfig.Tools...), tools...)
	// 清空静态列表，避免 SDK 在并发 Run 中复用同一份可变 ReAct 配置。
	cfg.ToolsConfig.Tools = nil
	// 通过 BeforeAgent 给每轮运行配置加入工具；此时不握手，也不发现工具。
	cfg.Handlers = append(cfg.Handlers, &fixedTools{tools: all})
}

// fixedTools 保存的是构造 Agent 时确定的工具列表，不保存某轮 WikiRoot。
type fixedTools struct {
	adk.BaseChatModelAgentMiddleware
	tools []tool.BaseTool
}

func (h *fixedTools) BeforeAgent(ctx context.Context, run *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	// run 属于本轮调用；保留其他 handler 加入的工具，再追加本次注册的工具。
	run.Tools = append(run.Tools, h.tools...)
	return ctx, run, nil
}
