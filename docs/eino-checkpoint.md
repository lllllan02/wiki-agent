# Eino Checkpoint 与暂停恢复

本项目使用 Eino ADK Runner 的 checkpoint 能力。`internal/agent/plugins/checkpoint` 是可选插件，只给 Runner 注册 CheckPointStore、checkpoint ID 和安全点取消，并在继续时调用 `Runner.Resume`。`WikiAgent` 仍负责 Wiki 输入、Eino 事件循环、流式输出和逐条消息处理。其他 Agent 可按相同方式注册插件，同时保留自己的执行逻辑。

## 运行约定

- 每个 Wiki 会话使用独立的 CheckPointID，Store 将 Eino 的检查点保存到本地文件。恢复时使用同一 ID 和 Store。
- 主动暂停使用 `CancelAfterChatModel | CancelAfterToolCalls`，等待当前模型调用或工具调用到达安全点。超时会升级为即时取消。
- 服务仅在确认检查点已落盘后标记为 `paused`。检查点缺失时显示失败，不宣称可以从断点继续。
- 正常完成后删除旧检查点；新的一轮执行开始前也清理旧检查点，避免误用上一次执行状态。
- `context.Canceled` 可能来自客户端断开或执行上下文结束，本身不证明存在可恢复检查点。

当前实现面向单机、只读任务的主动暂停。进程在模型或工具执行中途直接退出时，Eino 未必有可恢复的安全点；任务级持久执行恢复仍按 `dev.md` 的 L17 设计。

参考：[Eino ADK Agent Cancel and TurnLoop Quick Start](https://www.cloudwego.io/docs/eino/core_modules/eino_adk/agent_cancel_and_turnloop_quickstart/)、[Eino Human-in-the-Loop 技术文档](https://www.cloudwego.io/docs/eino/core_modules/eino_adk/agent_hitl/)。
