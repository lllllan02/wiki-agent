# Tool Registry：注册与调用

工具直接实现 EINO 的 `tool.InvokableTool`（`Info`、`InvokableRun`），流式工具使用 `tool.StreamableTool`。MCP 工具由官方适配器提供这些接口。注册层不再定义另一套工具接口，也不按工具名分支。

```text
LoadMCP → Manager.Tools ─┐
自定义 Go Tool ──────────┴→ RegisterTools → EINO Agent
模型 ToolCall → EINO 按名称查找 → 治理中间件 → 工具 → 治理中间件 → 统一工具结果
```

## 注册

在 `NewChatModelAgent` 之前注册一次：

```go
manager, err := registry.LoadMCP(appCtx, cfg.MCP)
if err != nil { return nil, err }
toolset, err := manager.SelectAvailable([]string{
    "filesystem__list_directory",
    "filesystem__search_files",
    "filesystem__read_text_file",
    "filesystem__get_file_info",
    "ripgrep__search",
    "files__read_file",
})
if err != nil { return nil, err }
schemaValidation := middleware.SchemaValidation(toolset.Tools)
middlewares := []compose.ToolMiddleware{
    middleware.ContractMiddleware(),
    schemaValidation,
    middleware.PathPolicy(toolset.Policies),
    middleware.Timeout(cfg.MCP.Timeout),
}
registry.RegisterTools(agentConfig, toolset.Tools, middlewares...)
agent, err := adk.NewChatModelAgent(appCtx, agentConfig)
```

`Manager` 维护应用生命周期内发现到的全部 MCP 工具；每个 Agent 通过 `Select` 或 `SelectAvailable` 明确声明自己被授予的工具集。`Select` 要求清单中的工具全部存在，`SelectAvailable` 适合允许本地配置裁剪的可选工具集。`RegisterTools` 合并配置中已有的工具，保留已有 handler 与中间件。工具名称查找、重复名称检查、ToolCall ID 关联和调用调度使用 EINO 本身的实现。

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

当前默认安装一组治理中间件，按下面顺序注册：

1. `ContractMiddleware`：最外层，把成功、错误和 panic 统一转换成 JSON 契约，不改变工具返回文本的长度和内容。
2. `SchemaValidation`：根据工具 `Info()` 暴露的 schema 检查 JSON 对象、必填字段、顶层未知字段、基础类型和枚举；失败时不进入工具。
3. `PathPolicy`：消费 MCP 注册表的 `path_parameters`，把相对路径改写到本轮 Wiki 根目录，拒绝越界路径和非 Markdown 文件。
4. `Timeout`：最贴近真实调用，用 `mcp.timeout` 限制单次工具调用。

因此 L03/L04 的统一入口不是项目自建 `execute_tool`，而是 EINO ToolNode 加这条中间件链。EINO 仍负责工具名称查找、ToolCall ID 关联和调用调度；项目只维护横切治理逻辑。

## 连接生命周期与目录职责

- `manager.go`：加载 MCP，按应用生命周期及配置共享连接；失败不缓存，应用退出时关闭，`Tools()` 返回列表副本。
- `eino.go`：合并工具和原生中间件，通过每轮 handler 注入工具。
- `internal/tool/middleware`：按文件拆分治理中间件；`contract.go`、`schema.go`、`path_policy.go`、`timeout.go` 分别实现独立职责。具体 Agent 在构造时选择需要的工具集和中间件顺序。
- `internal/tool/mcp`：部署配置、协议连接、工具发现及命名空间。

新增普通工具只需实现框架接口并注册；新增横切行为只需提供中间件。直接调用 `InvokableRun` 会跳过配置在 EINO Agent 上的中间件，正常业务应通过 Agent 调用。

MCP 配置中的 `path_parameters` 会进入 Wiki 路径策略；`mcp.timeout` 同时用于 MCP 连接初始化和单次工具调用。模型可以传当前 Wiki 内的相对路径，治理层会在调用上游 MCP 前改写为绝对路径。结果长度、摘要和上下文预算管理不属于当前注册层，后续由专门组件或中间件处理。
