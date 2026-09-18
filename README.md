# Wiki Agent

这是一个教学向的本地 Wiki Agent 项目，用 Go、CloudWeGo EINO 和 Gin 从最小 Agent Loop 开始，逐步搭建能读取、整理和修改 Markdown 笔记的 Agent Harness。

启动方式：在项目根目录执行 `go run ./cmd/wiki-agent`；首次运行会生成 `config.yaml`，填好模型配置后重新启动，再打开 `http://127.0.0.1:8080`，在页面中选择知识库目录后使用。
