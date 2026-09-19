# Tool Registry：注册与调用

工具直接实现 EINO 的 `tool.InvokableTool`（`Info`、`InvokableRun`），流式工具使用 `tool.StreamableTool`。MCP 工具由官方适配器提供这些接口。注册层不再定义另一套工具接口，也不按工具名分支。

```text
LoadMCP → Manager.Tools ─┐
自定义 Go Tool ──────────┴→ RegisterTools → EINO Agent
模型 ToolCall → EINO 按名称查找 → 中间件前置 → 工具 → 中间件后置 → 工具结果
```

## 注册

在 `NewChatModelAgent` 之前注册一次：

```go
manager, err := registry.LoadMCP(appCtx, cfg.MCP)
if err != nil { return nil, err }
tools, err := manager.Tools()
if err != nil { return nil, err }
// 自定义工具也直接追加到 tools。
registry.RegisterTools(agentConfig, tools)
agent, err := adk.NewChatModelAgent(appCtx, agentConfig)
```

`RegisterTools` 合并配置中已有的工具，保留已有 handler 与中间件。工具名称查找、重复名称检查、ToolCall ID 关联和调用调度使用 EINO 本身的实现。

当前 EINO v0.9.19 的静态 ReAct 配置存在并发运行状态复用问题，因此通过 `BeforeAgent` 在每轮注入同一组工具对象，让框架构造独立的运行配置。工具对象必须支持并发调用；注册层只保存列表副本，不保存单次调用状态。

## 前置与后置操作

直接传入原生 `compose.ToolMiddleware`，无需包装每个工具：

```go
middleware := compose.ToolMiddleware{
    Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
        return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
            // 前置操作；input 带工具名、Arguments、CallID 和 CallOptions。
            output, err := next(ctx, input)
            // 后置操作；这里同时能处理成功和错误。
            return output, err
        }
    },
}
registry.RegisterTools(agentConfig, tools, middleware)
```

多个中间件按注册顺序进入、反序退出：`A 前置 → B 前置 → 工具 → B 后置 → A 后置`。前置操作可以直接返回错误以停止执行；需要在 panic 时也执行的清理使用 Go 的 `defer`。流式工具使用 `Streamable`，增强工具使用 `EnhancedInvokable` / `EnhancedStreamable`；流式调用的 `next` 返回流，消费完成后的处理应围绕流的读取和关闭实现。

默认不安装中间件。参数、context、选项、返回值和错误交给 EINO 和工具自身处理；注册层不做 JSON 校验、Wiki 路径改写、权限检查、逐次调用超时或统一结果包装。后续按需要通过中间件补充。

## 连接生命周期与目录职责

- `manager.go`：加载 MCP，按应用生命周期及配置共享连接；失败不缓存，应用退出时关闭，`Tools()` 返回列表副本。
- `eino.go`：合并工具和原生中间件，通过每轮 handler 注入工具。
- `internal/tool/mcp`：部署配置、协议连接、工具发现及命名空间。

新增普通工具只需实现框架接口并注册；新增横切行为只需提供中间件。直接调用 `InvokableRun` 会跳过配置在 EINO Agent 上的中间件，正常业务应通过 Agent 调用。

MCP 配置中的 `path_parameters` 和 `mcp.max_bytes` 暂时保留兼容，但当前注册链路不消费它们。调用文件工具时应传明确的绝对路径；工具返回其自身结果，不再生成 `status/data/continuation` 契约。`mcp.timeout` 仍用于 MCP 连接初始化，具体传输行为由 MCP 层负责。
