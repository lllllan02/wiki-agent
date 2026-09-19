# 配置

程序启动时读取项目根目录的 `config.yaml`。如果文件不存在，会自动生成一个模板；填写模型配置后重新启动服务即可。知识库目录不在配置文件里；用户在页面中选择，多个浏览器窗口可分别选择项目并保存对话。

最小配置示例：

```yaml
model:
  api_key: "你的模型 API Key"
  name: "deepseek-v4-flash"
  base_url: "https://api.deepseek.com/v1"
  timeout: 60s

agent:
  max_steps: 8

server:
  address: "127.0.0.1:8080"

mcp:
  registry_file: mcp.yaml
  timeout: 30s
```

字段说明：

| 配置 | 默认值 | 作用 |
|---|---:|---|
| `model.api_key` | 空 | 模型服务密钥，必须填写 |
| `model.name` | 空 | 支持工具调用的模型名称，必须填写 |
| `model.base_url` | 空 | 完整的 OpenAI 兼容 API 地址；DeepSeek 示例为 `https://api.deepseek.com/v1`，程序不自动补充 `/v1` |
| `model.timeout` | `60s` | 单次模型请求超时 |
| `agent.max_steps` | `8` | 一轮问题最多调用模型的次数 |
| `server.address` | `127.0.0.1:8080` | 网页服务监听地址 |

直接启动网页服务：

```sh
go run ./cmd/wiki-agent
```

修改配置后重新启动服务，然后在浏览器打开 `http://127.0.0.1:8080`。页面中可输入知识库目录或从最近打开的目录中选择。默认共享 MCP 在创建 Agent 时连接并注册；后续窗口和其他 Agent 复用工具与连接。所有 MCP 均共享，不再配置目录连接数量上限。

配置来源只有 YAML 和代码中的默认值，优先级是 `YAML > default 标签`。未声明的旧字段或未知字段会被忽略；新增字段没有写入 YAML 时使用代码里的默认值。配置文件可能包含密钥，已被 Git 忽略，不要提交或分享。

## MCP 配置

`mcp.registry_file` 必填，留空会在启动时提示配置；`mcp.timeout` 默认 `30s`。`registry_file` 相对路径以 `config.yaml` 所在目录为基准。指定 `mcp.yaml` 后使用注册表中的已启用服务，现成服务器需要先安装。

服务启停、命令、地址和工具白名单在注册表中配置；敏感认证头写到被忽略的 `mcp.local.yaml`，并让 `registry_file` 指向该文件。产品配置仍只来自 YAML，不自动读取环境变量覆盖。详见 [MCP 接入说明](mcp.md)。
