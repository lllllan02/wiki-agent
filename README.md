# Wiki Agent

这是一个教学向的本地 Wiki Agent 项目，用 Go、CloudWeGo EINO 和 Gin 从最小 Agent Loop 开始，逐步搭建能读取、整理和修改 Markdown 笔记的 Agent Harness。

启动方式：在项目根目录执行 `go run ./cmd/wiki-agent`；首次运行会生成 `config.yaml`，填好模型配置后重新启动，再打开 `http://127.0.0.1:8080`，在页面中选择知识库目录后使用。

现成 MCP 已登记在 [mcp.yaml](mcp.yaml)。执行 `npm ci --prefix mcp`，并在 `config.yaml` 设置 `mcp.registry_file: mcp.yaml` 后，启用文件读取、列目录和正文搜索。完整步骤见 [MCP 接入说明](docs/mcp.md)，最终选型与后续自研清单见 [Agent Tool 清单](docs/agent-tools.md)。
