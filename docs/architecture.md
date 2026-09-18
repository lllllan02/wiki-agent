# 项目目录与设计说明

## MCP 提前接入后的变化

现已复用 EINO 官方 MCP Tool 适配器接入外部服务器。`internal/tool/mcp` 加载 `mcp.yaml`，提供 stdio / Streamable HTTP、工具白名单、命名空间、目录门控和调用限制；外部工具实现仍来自依赖包。`mcp/` 保存 Node.js 依赖锁和 Git MCP 的 Python 依赖声明。

模型客户端长期复用；启用 MCP 时，每轮运行按当前 Wiki 创建独立连接、工具集合和 EINO Agent，结束后释放。默认注册表启用 Filesystem 和 ripgrep；启用 Filesystem 读取后不重复开放原 `read_note`。没有配置 MCP 时继续使用 L01 原路径。

下文的「L01 实际目录」及单工具说明保留为初始架构背景；当前接入细节与限制以 [MCP 接入说明](mcp.md) 为准。提案、受控写入、持久恢复与多 Agent 尚未实现。

## 入口与核心的关系

当前唯一入口是本地 Web 页面。浏览器负责选择知识库目录、输入和展示，Gin 服务负责 HTTP 路由与会话，WikiAgent 负责把请求交给 EINO Agent 执行；页面只保存当前会话选择的目录，不直接执行工具，也不判断文件权限。

## L01 实际目录

```text
wiki-agent/
├── cmd/
│   └── wiki-agent/main.go         # 加载配置并启动应用
├── internal/
│   ├── app/
│   │   └── app.go                 # 启动并关闭本地 Web 服务
│   ├── config/
│   │   ├── config.go              # 配置分组、Viper YAML 加载和校验
│   │   ├── file.go                # 首次启动生成 YAML，不覆盖已有配置
│   │   ├── defaults.go            # 反射解析 default 标签
│   │   └── config_test.go         # 默认值、覆盖优先级及错误配置测试
│   ├── web/
│   │   ├── server.go              # Gin 路由、聊天 API 和内存会话
│   │   ├── static/index.html      # 独立前端页面、样式和交互脚本
│   │   └── server_test.go         # 页面、会话 Cookie 和请求校验
│   ├── agent/
│   │   ├── wiki_agent.go          # WikiAgent：创建模型和 EINO Agent，每轮创建 Runner 执行
│   │   ├── prompt.go              # 系统规则与资料边界
│   │   ├── wiki_agent_test.go        # WikiAgent 基础错误验证
│   │   ├── wiki_agent_openai_test.go # 本地 HTTP 协议集成测试
│   │   └── wiki_agent_url_test.go    # 模型 BaseURL 清洗测试
│   └── tool/
│       └── wiki/
│           ├── read_note.go       # EINO read_note 工具、限目录读取及输出上限
│           └── read_note_test.go  # 文件边界验证
├── wiki/notes/cache.md            # 可直接运行的示例资料
├── docs/
│   ├── architecture.md            # 当前目录职责与设计说明
│   └── configuration.md           # 配置机制及扩展教程
├── dev.md                         # 建设清单
├── README.md                      # 项目说明和启动方式
├── config.example.yaml            # 中文配置示例
├── Makefile
├── go.mod
└── go.sum
```

`cmd` 只负责入口：创建或加载配置，然后把配置交给 `internal/app`。`app` 只管理本地 HTTP server 的启动和关闭，因为这是它必须负责释放的资源。

`internal/agent` 里的 `WikiAgent` 是长期复用对象。创建 `WikiAgent` 时会直接调用 EINO OpenAI 兼容适配器创建模型客户端，并创建 EINO `ChatModelAgent`；项目没有单独的 `model` 目录，因为当前没有额外抽象，只是在 Agent 创建阶段使用框架提供的模型构造函数。每次 `RunWithHistory` 时再用这个 Agent 创建一个新的 EINO `Runner` 来执行本轮请求。这样模型和 Agent 不会每轮重复初始化，而 Runner 也不会把单次执行状态带到下一轮。

`read_note` 是一个 EINO `InvokableTool`。工具对象在 Agent 创建时注册一次，当前 Web 会话选择的知识库目录通过 EINO `tool.Option` 在每次运行时传入。这样同一个 Agent 可以服务不同会话和不同目录，同时工具仍然只允许读取本轮选择目录内的 Markdown 文件。

L01 使用 EINO 的 Agent/Runner 层：模型接口、OpenAI 兼容适配器、ADK ChatModelAgent、ADK Runner、ToolsNode 和工具 Schema。循环停止、工具执行、工具结果回填由 EINO 管理，项目代码只保留配置、Web 会话、工具实现和结果适配。

## 演进原则

目录跟着真实代码演进，不提前创建空包或占位接口。后续如果某类职责已经在两个以上地方重复出现，再把它抽成独立包；如果只是路线图上的未来能力，先留在开发清单里，不写进架构目录。

测试夹具进入 `testdata`；运行产生的数据未来放入独立且忽略提交的 `var`；具体使用哪个知识库目录由 Web 会话打开，不写在启动配置里。未来增加公共 SDK 时才考虑 `pkg`，当前所有 Go 业务包保持内部可见。

## 请求失败时的信息

`RunWithHistory` 成功时返回内存中的 `Result`：

- `Messages` 保存当前 Web 会话的用户消息和最终回答，用于连续追问。
- `Answer` 仅在 EINO Runner 产出最终 assistant 消息后设置。

Web 层把错误整理成可读提示返回给页面；失败轮不加入后续对话历史，避免下一次请求带入未配对的工具消息。这里不是 L05 的完整运行状态机，也没有 L15 的持久化；进程退出后不能恢复。

## 教学注释约定

代码使用中文说明模块职责、关键步骤、设计原因及适用边界。比如解释为什么配置只使用 YAML、为什么目录通过运行时 option 传给工具、为什么目录前缀判断不能代替受限目录句柄。测试同样说明它保护的行为，让读者可以把测试当作可执行的设计说明。

配置对象通过 `NewDefaults[T]()` 或 `Load()` 创建，默认值只定义在字段标签中。新增配置时同步更新中文字段注释、YAML 示例和配置教程；不在构造函数、Web 入口和模型层重复维护默认常量。

## Web 会话边界

Web 页面每轮等待一个完整回答后再接收下一轮任务。当前知识库目录和成功消息历史按浏览器 Cookie 区分，只保存在当前进程内；选择目录只更新会话状态，真正读取时由 read_note 工具校验目录和文件；切换目录会清空该会话历史，服务重启后不恢复。当前不实现运行中追加要求、暂停、手动清空会话或上下文压缩，这些仍按后续阶段建设。
