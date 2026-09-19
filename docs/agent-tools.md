# Agent Tool 最终选型与迭代清单

更新：2026-09-19。依据 [dev.md](../dev.md) 的 Wiki 功能范围整理；仓库没有独立的 BV 文档。

**原则：有合适的 MCP 就直接复用，只开放所需能力。权限、范围、版本和可靠性要求由 Agent 的工具执行层处理；只有没有可复用实现的业务缺口才新增 Tool。**

## 1. 直接使用的 MCP

以下 MCP 已登记在 [mcp.yaml](../mcp.yaml)，通过 EINO 官方 MCP 适配器发现工具并交给 Agent。注册、安装、启用是不同状态：禁用项不会启动进程、连接外部服务或进入模型工具列表。

| 能力 | 选用 MCP | 当前开放范围 | 当前状态 |
|---|---|---|---|
| 文件读取、列目录、文件名搜索 | [Filesystem MCP](https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem) | `list_directory`、`search_files`、`read_text_file`、`get_file_info`；路径直接传给上游 | 已注册、安装，默认启用 |
| 长笔记按行续读 | [Files MCP](https://github.com/Abhishekkumar2021/mcp-suite/tree/main/servers/files) | 开放 `read_file` 的 `offset`/`limit` 行窗口 | 已注册、安装，默认启用 |
| 笔记正文搜索 | [ripgrep MCP](https://github.com/mcollina/mcp-ripgrep) | `search`；使用上游参数与返回值 | 已注册、安装，默认启用 |
| 联网搜索、网页提取 | [Tavily MCP](https://docs.tavily.com/documentation/mcp) | `tavily-search`、`tavily-extract` | 已注册，待配置凭据后启用 |
| 浏览器观察与交互 | [Playwright MCP](https://github.com/microsoft/playwright-mcp) | 预登记导航、标签页、页面观察、关闭；点击、输入、截图后续按能力开放 | 已注册、安装，暂不启用 |
| Git 历史与差异 | [Git MCP](https://github.com/modelcontextprotocol/servers/tree/main/src/git) | 状态、日志、暂存/未暂存差异；不开放提交、重置等写操作 | 已注册、安装，Git 知识库按需启用 |
| 长期记忆 | [Memory MCP](https://github.com/modelcontextprotocol/servers/tree/main/src/memory) | 预登记 `search_nodes`、`open_nodes`；记忆写入在 L19 开放 | 已注册、安装，暂不启用 |

Filesystem 已有创建、编辑、移动能力，后续完善写入门控后开放其工具，不再重写文件工具。Playwright 已有浏览器动作能力，不自建浏览器操作引擎。远程浏览器继续使用 Browserless 作为运行服务，不另造一套 Tool。


暂不重复接入 Fetch、Firecrawl；确有 Tavily 提取无法覆盖的需求再补。PDF/Office 导入不属于当前必需功能，未来需要时直接接 MarkItDown MCP。

## 2. 留到后续迭代的业务 Tool

**这是一份待评估清单，不是必须逐个实现的开发承诺。本次没有实现下列 Tool。** 每轮开始先检查 MCP、EINO 或已有中间件能否直接承担，能复用就从自研清单删掉。

| 能力组 | 候选 Tool 名称 | 真正需要自行负责的部分 | 迭代 / 状态 |
|---|---|---|---|
| 变更提案与交付 | `propose_changes`、`get_changeset`、`apply_changeset` | 提案状态、审阅内容与具体操作的绑定；实际文件操作调用 Filesystem MCP | L10–L12、L18；待评估 |
| Wiki 业务检查 | `validate_changeset` | 修改范围、受影响引用、项目特定的验收规则；链接与文件数据复用 MCP | L16–L18；待评估 |
| 任务与计划 | `update_plan`、`get_task` | 本产品任务、依赖与完成依据；优先复用 EINO，只有模型需要管理状态时才加 Tool | L16–L17；待评估 |
| 子 Agent 管理 | `spawn_agent`、`get_agent_result`、`send_agent_message`、`cancel_agent` | 封装 EINO 子任务能力，补父子预算、归属与产物接纳；固定流程可能不需要暴露这些 Tool | L24–L25；待评估 |
| 定期维护管理 | `create_schedule`、`list_schedules`、`update_schedule`、`delete_schedule` | 将本产品任务接到现成调度器；若只从 UI 配置，先不增加模型 Tool | L26；待评估 |

L02 使用已登记的 Filesystem 和 ripgrep MCP 完成列目录、文件名搜索、正文搜索与读取。移除了本地 `read_note`、`list_notes` 和 `search_notes`，避免维护重复的文件工具。L03 使用现成 Files MCP 的行窗口分页续读长笔记，没有新增 Go 文件读取工具。文件编辑、Git 操作、网页搜索、抓取、浏览器动作、记忆 CRUD 和反向链接继续按本表复用 MCP；业务 Tool 仍在需要其产品状态与规则的阶段实现。

## 3. 统一接入层负责什么

本次已完成：stdio / Streamable HTTP 接入、显式工具白名单、服务器命名空间、EINO 原生中间件接入、缺失工具报错，以及 Agent 创建时注册工具、共享服务跨目录和 Agent 复用、应用退出时关闭。使用官方 [EINO MCP 适配器](https://github.com/cloudwego/eino-ext/tree/main/components/tool/mcp)，没有重写 MCP 协议或上述外部工具。

后续迭代：写入授权、提案版本绑定、去重、结果未知核对、多文件恢复、浏览器访问/动作权限。它们是执行层机制，不能靠 MCP 自带的能力标签或模型提示词代替，也不必全部做成新的 Tool。

详细安装、配置、验证与当前边界见 [MCP 接入说明](mcp.md)。原 dev.md 的教学路线保留：本次提前完成 MCP 注册与基础接入，不表示 L10–L26 的业务和可靠性验收已经完成。
