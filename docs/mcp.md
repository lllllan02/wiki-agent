# MCP 注册与使用

本项目消费现成 MCP Server。`internal/tool/mcp` 负责连接与工具发现，`internal/tool/registry` 负责模型调用后的参数与权限检查，不实现文件、搜索、浏览器或 Git 功能。

注册层为什么这样拆分、如何管理共享状态和执行约束，见 [工具注册层详解](tool-registry.md)。

## 安装和开启

Node.js 服务器版本与传递依赖锁定在 `mcp/package.json`、`mcp/package-lock.json`。需要 Node.js 及 PATH 中的 `rg`。

```sh
npm ci --prefix mcp
```

Git MCP 使用独立 Python 环境，避免修改全局 Python：

```sh
python3 -m venv .cache/mcp-python
.cache/mcp-python/bin/python -m pip install -r mcp/requirements.txt
```

`mcp/requirements.txt` 固定 Git MCP 直接依赖版本；它不是全部 Python 传递依赖的锁文件。

在本地 `config.yaml` 中设置：

```yaml
mcp:
  registry_file: mcp.yaml
  timeout: 30s
  max_bytes: 32768
```

默认配置在创建 Agent 时连接并发现共享 MCP，此后所有窗口、目录和 Agent 复用工具对象与连接。所有服务统一共享，不按目录启动。`registry_file` 必须指定；留空会明确报错。已有用户配置不会被程序自动覆盖。

首次自动生成的配置保留空的 `registry_file`，需安装并显式设置后启动。仓库注册表默认启用 Filesystem、ripgrep 和 Files，共 6 个只读工具。

## 注册表

`mcp.yaml` 已登记 7 个服务。需要填写密钥或部署地址时，复制为被 Git 忽略的 `mcp.local.yaml`，将 `config.yaml` 的 `registry_file` 改为此文件；凭据只在本地文件中配置。

| 字段 | 含义 |
|---|---|
| `name` | 唯一服务器名，工具暴露为 `服务器名__原工具名` |
| `enabled` | 是否启动、发现并交给 Agent；禁用项只作登记 |
| `transport` | `stdio` 或 `streamable_http` |
| `command` / `args` / `env` | 本地服务启动参数；直接启动进程，不通过 shell 拼接 |
| `url` / `headers` | HTTP 服务地址和认证头；不回显到模型或错误页面 |
| `tools` | 必须显式列出使用的工具；空列表报错，不自动开放全部能力 |
| `path_parameters` | 需要绑定当前 Wiki 的顶层路径参数名；当前只用于已存在的文件/目录 |

本地命令参数只支持 `${PROJECT_ROOT}`（注册表所在目录）。知识库目录通过每次工具调用的参数传入，不支持启动时绑定 `${WIKI_ROOT}`。不要把注册表放到任意位置后仍假设 PROJECT_ROOT 指向仓库。

- Filesystem / ripgrep：默认启用；Filesystem 读取仅限 `.md`。ripgrep 仅开放基础 `search`，强制 Markdown、关闭颜色并限制参数；其 `maxResults` 是每个文件的匹配上限，最终工具输出另有总字节上限。
- Files：默认只读启用，只开放 `read_file` 的行窗口分页；启动根目录设为 `/` 以支持多个 Wiki，实际访问仍由逐次调用的路径检查限制在当前 Wiki。`offset` 从 1 开始，`limit` 最多 200 行。
- Tavily：在本地副本中填写认证头，再设 `enabled: true`。注册不代表凭据已验证。
- Playwright：已安装 MCP Server，选择系统 Chrome。当前只预登记基本观察工具；浏览器进程、页面交互和站点限制还须在 L23 验收。
- Git：安装上述 Python 环境后，仅在当前 Wiki 是 Git 仓库时启用。不能将“允许本仓库路径”理解为已具备不可信 Git 配置的隔离沙箱。
- Memory：启用后使用服务目录中的 `var/memory.jsonl`，所有 Wiki 共用这份应用记忆；当前只有查询工具，增删改留到 L19。

连接失败或白名单工具缺失会明确报错，不静默丢弃服务后假装该轮工具完整可用。

## 生命周期与边界

WikiAgent 构造时调用 `LoadMCP(appCtx, cfg.MCP)` 获取共享工具管理器。工具模块以应用生命周期（context 的 Done 通道）和 MCP 配置为共享键；首次调用连接服务、握手并发现工具，后续 Agent 获取同一个 Manager 和相同工具对象。并发构造也只初始化一次。

Agent 构造时调用 `manager.Tools()`，再用 `registry.RegisterTools(agentConfig, tools)` 注册。`Stream` 只创建流式 Runner，不获取或释放连接。目录来自运行 context，在实际调用前检查；打开或切换目录不会改变 MCP 连接。

应用生命周期 context 取消后，工具模块移除共享实例并关闭整组连接。单轮调用取消不会关闭共享连接。初始化失败会清理已建立的连接且不缓存失败结果，后续可以重试。单个 Agent 不得关闭共享 Manager；新应用生命周期会重新建立连接。配置或工具 schema 变更需重启应用。

EINO v0.9.19 直接静态注册工具后，同一 Agent 并发 Run 会竞争内部 ReAct 的 `cancelCtx`。`RegisterTools` 将同一组标准工具注册到固定 handler 中，使 SDK 为每轮构建独立执行配置；handler 不执行工具发现、连接打开或目录包装。这是 SDK 适配，不要求各 Agent 实现自己的工具生命周期。

Filesystem MCP 目前以 `/` 作为进程启动根目录，因此跨目录复用不依赖重启；逐次访问范围由 Agent 工具执行层按真实路径限制。此进程本身拥有较宽的文件访问能力，只供本地 Agent 独占连接。当前执行层拒绝越界和符号链接逃逸；这是调用前检查，不是恶意并发改写路径下的 OS 沙箱。当前知识库仍按原项目约定由用户控制。未来放开写入、浏览器动作和不可信服务时，再补相应权限与隔离机制。

L03 后，进入模型的工具结果统一为固定字段的 JSON。`status` 为 `ok`、`empty`、`error` 或 `truncated`；`data` 始终是对象，其中 `text` 是完整文本或明确标记的前缀，`structured` 保留 MCP 的 `structuredContent`（没有时为 `null`）；`error` 是带 `code`、`message` 的对象或 `null`；`source` 是工具名；`truncated` 是布尔值；`continuation` 是带 `tool`、`arguments`、`hint` 的对象或 `null`。例如分页读取后的结果包含 `"continuation":{"tool":"files__read_file","arguments":{"path":"note.md","offset":51,"limit":50}}`，模型可直接按参数继续调用。参数先按 MCP 公布的 schema 检查顶层必填字段、类型与基本范围，再执行 Wiki 路径、Markdown 类型和搜索限制等业务检查；复杂组合 schema 仍由上游服务验证。`mcp.max_bytes` 限制 `data.text` 与 `data.structured` 的总字节数，包装本身会额外占用少量字节，也不限制上游生成结果的内存。长 Markdown 可用现成 Files MCP 的 `files__read_file` 按 1-based `offset` 和 `limit`（最多 200 行）分页读取；若单页仍超限，应缩小 `limit` 重试。其他长结果需缩小查询范围。EINO 当前适配器把 MCP 结果序列化成文本，截图多模态接入尚未验收，因此没有将截图工具加入现有白名单。

## 验证

普通测试使用本机 HTTP MCP 测试服务器验证认证、工具白名单、缺失工具及结果截断，不依赖外部服务。

```sh
go test ./...
```

安装 Node.js 服务器后可显式运行真实本地 MCP 验收：

```sh
WIKI_AGENT_MCP_INTEGRATION=1 go test ./internal/tool/mcp -run TestInstalledServers -v
```

这些环境变量只控制测试，不覆盖产品 YAML 配置。验证包含 Filesystem 发现/读取/文件搜索、ripgrep 正文搜索、目录边界、两份 Wiki 隔离，以及 Memory/Playwright 工具发现；安装 Python 环境后还验证 Git MCP 的发现和临时仓库状态查询。它不代表真实模型语义、网页导航或外部联网服务已经验收。

2026-09-19 已通过全项目测试和上述 5 个本地服务器的集成检查；另通过本地模型协议夹具验证了“模型请求 MCP Tool → 服务执行 → 结果回填 → 最终回答”的链路。Tavily 未连接真实服务，注册项保留禁用。

### MCP 复用边界

- Filesystem：启动目录是服务允许访问的范围。当前以 `/` 启动共享进程，执行层按本轮 Wiki 检查每个路径。不要在并发运行中修改服务全局 roots 来切换 Wiki。
- ripgrep：每次调用传入 `path`，可以共享连接。
- Git：已安装版本的 `--repository` 是可选的范围限制；省略后每次调用使用 `repo_path`。所有目录复用同一个服务，执行层继续校验路径。
- Tavily：无本地目录绑定，可共享连接。Memory：示例使用应用级固定存储文件，由所有 Agent 和 Wiki 共用。
- Playwright：进程可复用，但浏览器页面和登录状态也会共享；当前仍禁用，会话隔离属于后续浏览器功能设计。

注册适配使用 EINO v0.9.19 的 [BeforeAgent 接口](https://github.com/cloudwego/eino/blob/v0.9.19/adk/chatmodel.go)，每轮注入的是同一组工具对象。已移除 Obsidian 注册项及目录绑定机制。旧配置中的 `scope`、`bound_wiki`、`mcp.max_bound_roots` 和 `agent.max_cached_wikis` 字段需删除。

### 复用到其他 Agent

```go
// 在使用工具的组件内获取；所有 Agent 传入同一个应用生命周期 appCtx。
// 相同配置返回同一 Manager，不要传单轮请求 context。
manager, err := registry.LoadMCP(appCtx, cfg.MCP)
if err != nil { return err }
tools, err := manager.Tools()
if err != nil { return err }
registry.RegisterTools(agentConfig, tools)
// 创建 Agent 后，运行时只需携带调用上下文：
runCtx := runcontext.With(ctx, runcontext.Metadata{SessionID: sessionID, WikiRoot: wikiRoot})
runner := adk.NewRunner(runCtx, adk.RunnerConfig{Agent: myAgent})
iter := runner.Run(runCtx, messages)
```

`manager.Tools()` 返回标准 `[]tool.BaseTool`，可筛选或与普通 Go 工具一起注册。无路径参数的共享工具不要求 Wiki 上下文。子 Agent 继承上下文即可，不需要在自己的 Run 中增加 Open/Release。
