# 配置

程序启动时读取项目根目录的 `config.yaml`。如果文件不存在，会自动生成一个模板；填写模型配置后重新启动服务即可。知识库目录不写在配置文件里，进入网页后由当前浏览器会话选择。

最小配置示例：

```yaml
model:
  api_key: "你的模型 API Key"
  name: "deepseek-v4-flash"
  base_url: "https://api.deepseek.com/v1"
  timeout: 60s

wiki:
  max_read_bytes: 16384

agent:
  max_steps: 8

server:
  address: "127.0.0.1:8080"
```

字段说明：

| 配置 | 默认值 | 作用 |
|---|---:|---|
| `model.api_key` | 空 | 模型服务密钥，必须填写 |
| `model.name` | 空 | 支持工具调用的模型名称，必须填写 |
| `model.base_url` | 空 | OpenAI 兼容 API 地址；DeepSeek 使用 `https://api.deepseek.com/v1` |
| `model.timeout` | `60s` | 单次模型请求超时 |
| `wiki.max_read_bytes` | `16384` | 单次读取笔记的字节上限；不表示知识库目录 |
| `agent.max_steps` | `8` | 一轮问题最多调用模型的次数 |
| `server.address` | `127.0.0.1:8080` | 网页服务监听地址 |

直接启动网页服务：

```sh
go run ./cmd/wiki-agent
```

修改配置后重新启动服务，然后在浏览器打开 `http://127.0.0.1:8080`。页面顶部填写后端能访问的本机目录并点击“打开”，当前浏览器会话会切换到该目录；切换目录会清空该会话的历史。

配置来源只有 YAML 和代码中的默认值，优先级是 `YAML > default 标签`。配置文件可能包含密钥，已被 Git 忽略，不要提交或分享。
