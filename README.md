# Wiki Agent

这是一个教学向的本地 Wiki Agent 项目，用 Go、CloudWeGo EINO 和 Gin 从最小 Agent Loop 开始，逐步搭建能读取、整理和修改 Markdown 笔记的 Agent Harness。

启动方式：在项目根目录执行 `go run ./cmd/wiki-agent`；首次运行会生成 `config.yaml`，填好模型配置和 MCP 注册表后重新启动，再打开 `http://127.0.0.1:8080`，在页面选择知识库目录。多个窗口可以分别选择项目并保存对话。

文件、目录和正文搜索都由现成 MCP 提供，本项目只负责工具注册、范围约束和 Agent 调用。先执行 `npm ci --prefix mcp`，再在 `config.yaml` 设置 `mcp.registry_file: mcp.yaml`。现成 MCP 已登记在 [mcp.yaml](mcp.yaml)。完整步骤见 [MCP 接入说明](docs/mcp.md)，选型与后续清单见 [Agent Tool 清单](docs/agent-tools.md)。

工具注册层的源码导读与设计理由见 [工具注册层详解](docs/tool-registry.md)。
