# 项目目录与设计说明

## MCP 提前接入后的变化

现已复用 EINO 官方 MCP Tool 适配器接入外部服务器。`internal/tool/mcp` 加载 `mcp.yaml`，负责 stdio / Streamable HTTP 连接、工具发现和命名；`internal/tool/registry` 在模型调用和 MCP 执行之间校验参数、路径范围并限制调用。外部工具实现仍来自依赖包。`mcp/` 保存 Node.js 依赖锁和 Git MCP 的 Python 依赖声明。

WikiAgent 在构造时创建模型、从工具模块获取应用内共享的 MCP 管理器，并注册一次工具；默认 Filesystem 和 ripgrep 在此连接并发现，工具对象和 MCP 连接可供多个 Agent 复用。WikiAgent 只保留一个 EINO Agent，每轮通过上下文传入目录、创建支持流式输出的 Runner；运行方法不负责工具初始化或连接借用。

`internal/tool/registry` 负责工具注册、EINO 并发适配、目录上下文、路径检查及连接生命周期。所有 MCP 跨 Agent 和目录共享，同一应用生命周期、同一份配置只初始化一次，不维护目录连接池。未知名称与重复注册明确报错。原本地 `read_note`、`list_notes` 和 `search_notes` 已移除。

接入细节与限制见 [MCP 接入说明](mcp.md)，工具注册层的设计原因和源码阅读顺序见 [工具注册层详解](tool-registry.md)。提案、受控写入、持久恢复与多 Agent 尚未实现。

## 入口与核心的关系

当前唯一入口是本地 Web 页面。用户在页面选择知识库目录，页面保存最近打开的路径；Gin 服务负责 HTTP 路由与会话目录选择，并把目录和会话 ID 写入运行 context；WikiAgent 负责执行。

## 当前工具结构

`internal/tool/mcp` 从 `mcp.yaml` 连接现成服务并应用白名单；`internal/tool/registry` 包装已发现的 MCP 工具并注册到 EINO，启动时检查重名；运行时名称分发由 EINO 负责，执行包装层校验 Wiki 路径边界、参数、超时和输出。文件列表、文件名搜索、正文搜索和读取由 Filesystem 与 ripgrep MCP 实现。没有另写同功能的 Wiki Tool。

`cmd` 负责入口，`internal/app` 创建 Agent 和 HTTP 服务，`internal/web` 维护网页会话。每个 Agent 在构造时接入工具，仅保存 EINO Agent，运行接口为 `Stream(ctx, request, onText)`，运行时创建 Runner。页面打开目录时由 Web 层检查目录存在性。首个 Agent 构造时建立 MCP 连接，后续 Agent 复用；工具组件随应用生命周期 context 关闭，MCP 初始化失败时立即清理。EINO ADK 负责模型调用、工具调用标识及结果回填。

## 演进原则

目录跟着真实代码演进，不提前创建空包或占位接口。后续如果某类职责已经在两个以上地方重复出现，再把它抽成独立包；如果只是路线图上的未来能力，先留在开发清单里，不写进架构目录。

测试夹具进入 `testdata`；运行产生的数据未来放入独立且忽略提交的 `var`；知识库目录由页面选择，不写入启动配置。未来增加公共 SDK 时才考虑 `pkg`，当前所有 Go 业务包保持内部可见。

## 请求失败时的信息

`Stream(ctx, request, onText)` 在生成过程中回调当前助手正文，成功时返回包含最终 `Answer` 的 `Result`；失败时返回错误，由 Web 层作为流式错误事件提示。页面只调用 `/api/chat/stream`。

消息片段如何进入 `onText`、页面何时收到 `update`、工具调用由 EINO 在哪里判断，见[流式回答链路与时序图](streaming.md)。

`internal/runcontext` 在 Web、Agent 和工具之间传递 `SessionID` 与 `WikiRoot`，不传消息历史。当前尚未接入会话存储，每轮只将本次用户请求交给 Runner；后续可在 Agent 内按会话 ID（并结合 Wiki 边界）加载和保存历史。页面显示的旧消息不会自动成为模型上下文。

## 教学注释约定

代码使用中文说明模块职责、关键步骤、设计原因及适用边界。比如解释为什么配置只使用 YAML、为什么 MCP 连接在服务生命周期内复用、为什么目录前缀判断不能代替受限目录检查。测试同样说明它保护的行为，让读者可以把测试当作可执行的设计说明。

配置对象通过 `NewDefaults[T]()` 或 `Load()` 创建，默认值只定义在字段标签中。新增配置时同步更新中文字段注释、YAML 示例和配置教程；不在构造函数、Web 入口和模型层重复维护默认常量。

## Web 会话边界

Web 页面逐段显示 Markdown 回答，每轮完成后再接收下一轮任务。页面在 `sessionStorage` 中保存对话 ID，在本地浏览器保存最近打开的目录；服务按 ID 分开目录选择，目前不保存或回传模型历史；真正执行前由工具注册表校验目录和文件，服务重启后不恢复。当前不实现运行中追加要求、暂停、手动清空会话或上下文压缩，这些仍按后续阶段建设。
