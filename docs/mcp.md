# MCP 注册与使用

本项目消费现成 MCP Server。`internal/tool/mcp` 仅负责连接和调用边界，不实现文件、搜索、浏览器或 Git 功能。

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

重启服务后，每轮任务按当前网页选择的知识库连接已启用 MCP。`registry_file` 留空保持原来的 `read_note` 行为；已有用户配置不会被程序自动覆盖。

首次自动生成的配置默认不启用外部进程；安装后设置 `registry_file` 即可。仓库注册表默认启用 Filesystem 和 ripgrep，共 5 个只读工具。

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
| `bound_wiki` | 固定远程 Wiki 对应的本地绝对目录；不匹配时拒绝连接 |

本地命令参数支持 `${PROJECT_ROOT}`（注册表所在目录）与 `${WIKI_ROOT}`（本轮选择并解析后的目录）。这两个占位符由程序替换，不是 shell 执行或模型变量。不要把注册表放到任意位置后仍假设 PROJECT_ROOT 指向仓库。

- Filesystem / ripgrep：默认启用；Filesystem 读取仅限 `.md`。ripgrep 仅开放基础 `search`，强制 Markdown、关闭颜色并限制参数；其 `maxResults` 是每个文件的匹配上限，最终工具输出另有总字节上限。
- Tavily：在本地副本中填写认证头，再设 `enabled: true`。注册不代表凭据已验证。
- Playwright：已安装 MCP Server，选择系统 Chrome。当前只预登记基本观察工具；浏览器进程、页面交互和站点限制还须在 L23 验收。
- Git：安装上述 Python 环境后，仅在当前 Wiki 是 Git 仓库时启用。不能将“允许本仓库路径”理解为已具备不可信 Git 配置的隔离沙箱。
- Memory：每个 Wiki 使用独立 `.wiki-agent-memory.jsonl`，当前只有查询工具；增删改留到 L19。
- Obsidian：部署 maxkuminov/obsidian-mcp 后配置 URL、认证和 `bound_wiki`。远程服务使用自己的 vault 映射，不会因为网页选择了新目录自动切库。

连接失败或白名单工具缺失会明确报错，不静默丢弃服务后假装该轮工具完整可用。

## 生命周期与边界

模型客户端继续复用。MCP 连接、工具集合和 EINO Agent 按单轮运行创建，运行结束或失败后取消并关闭连接，避免不同知识库共享可变 Roots。浏览器也因此不能跨轮保留登录态或页面；持久浏览器会话属于 L23 后续工作。

当前中间件按真实路径拒绝越界和符号链接逃逸；这是调用前检查，不是恶意并发改写路径下的 OS 沙箱。当前知识库仍按原项目约定由用户控制。未来放开写入、浏览器动作和不可信服务时，再补相应权限与隔离机制。

输出超限会返回 `truncated: true` 和前缀，提示缩小范围或使用上游分页参数。截断不自动产生续读游标，也不限制上游读取整个文件的内存；EINO 当前适配器把 MCP 结果序列化成文本，截图多模态接入尚未验收，因此没有将截图工具加入现有白名单。

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

2026-09-19 已通过全项目测试和上述 5 个本地服务器的集成检查；另通过本地模型协议夹具验证了“模型请求 MCP Tool → 服务执行 → 结果回填 → 最终回答”的链路。Tavily 与 Obsidian 未连接真实服务，注册项保留禁用。
