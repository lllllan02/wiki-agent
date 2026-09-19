package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

// executionTool 是模型调用和真实工具执行之间的程序边界。
// 这里使用会话选定的 Wiki，而不是依靠模型或 MCP Server 判断访问范围。
// 每个对象对应一个已发现的 MCP 工具；多个 Agent 可并发复用，字段构造后不再修改。
type executionTool struct {
	upstream       tool.InvokableTool
	info           *schema.ToolInfo
	pathParameters []string
	timeout        time.Duration
	maxBytes       int
}

// 工具定义不随目录改变，否则共享 Agent 会看到其他会话的目录状态。
// schema 只读；真正的目录检查留到 InvokableRun，届时才有本轮 context。
func (t *executionTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

// InvokableRun 的顺序是：取得本轮目录、检查并改写参数、限制具体工具能力、
// 带本轮超时调用上游，最后限制返回给模型的内容。每一步都只使用局部变量。
func (t *executionTool) InvokableRun(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
	// 只有声明了路径参数的工具才依赖 Wiki；网络搜索等工具不能被无关的目录条件阻挡。
	var root string
	if len(t.pathParameters) > 0 {
		var err error
		root, err = wikiPath(runcontext.From(ctx).WikiRoot)
		if err != nil {
			return "", err
		}
	}
	// schema 和提示词不能代替程序校验：模型输出仍可能是 null、数组或错误类型。
	// 在本轮局部 map 上改写参数，不能改共享工具对象，也不能直接转发未检查的原始 JSON。
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return "", fmt.Errorf("工具参数必须是 JSON 对象")
	}
	// 参数名来自受信任的部署配置。路径由“应用选定目录 + 模型相对路径”确定，
	// 不接受模型自行指定一个新的根目录；转换为绝对路径也避免依赖 MCP 进程工作目录。
	for _, key := range t.pathParameters {
		path, ok := args[key].(string)
		if !ok || strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("%s 必须是 Wiki 内的路径", key)
		}
		resolved, err := scopedPath(root, path)
		if err != nil {
			return "", err
		}
		args[key] = resolved
	}
	// 下面是当前教学项目的两条具体产品规则，不是假装通用的插件规则引擎。
	// 上游工具支持更多文件类型，但本阶段只承诺读取 Markdown，因而在统一出口收紧能力。
	if t.info.Name == "filesystem__read_text_file" {
		path, _ := args["path"].(string)
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return "", fmt.Errorf("当前 Wiki 只开放 Markdown 正文读取")
		}
	}
	if t.info.Name == "ripgrep__search" {
		pattern, ok := args["pattern"].(string)
		if !ok || strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "-") {
			return "", fmt.Errorf("搜索模式不能为空或以 - 开头")
		}
		// 限定 Markdown、关闭终端颜色，防止无关资料与转义序列占用模型上下文。
		// maxResults 是上游的每文件匹配上限，不是整个调用的总输出上限。
		args["filePattern"], args["useColors"] = "*.md", false
		for key, limit := range map[string]float64{"maxResults": 100, "context": 5} {
			v, exists := args[key]
			if !exists {
				if key == "maxResults" {
					args[key] = limit
				}
				continue
			}
			// encoding/json 将数字解为 float64；同时检查整数性，避免 1.5 被截成 1。
			n, ok := v.(float64)
			if !ok || n < 0 || n > limit || n != float64(int(n)) || (key == "maxResults" && n == 0) {
				return "", fmt.Errorf("%s 必须是允许范围内的整数，最大 %g", key, limit)
			}
		}
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	// 超时从请求 context 派生：保留会话信息，并同时响应用户取消与工具超时。
	// 只取消这次调用，不能取消 Manager 使用的应用 context，否则会关闭其他 Agent 的连接。
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	result, err := t.upstream.InvokableRun(callCtx, string(payload), opts...)
	if err != nil {
		if callCtx.Err() != nil {
			return "", fmt.Errorf("工具 %s 调用取消或超时: %w", t.info.Name, callCtx.Err())
		}
		// 外部错误可能回显认证信息或资料；只透传我们自己生成的工具名和通用提示。
		return "", fmt.Errorf("工具 %s 调用失败，请检查参数或服务状态", t.info.Name)
	}
	if len(result) <= t.maxBytes {
		return result, nil
	}
	// 按字节限制进入模型的资料量，但 UTF-8 汉字可能占多个字节，不能留下半个字符。
	// 用显式标记包住片段，避免模型把被截断的 JSON 或文本误当成完整结果。
	// MaxBytes 限制的是原结果前缀；JSON 包装与转义会增加长度，也不限制上游生成结果的内存。
	result = result[:t.maxBytes]
	for !utf8.ValidString(result) && len(result) > 0 {
		result = result[:len(result)-1]
	}
	bounded, _ := json.Marshal(map[string]any{"truncated": true, "content_prefix": result, "notice": "工具结果超过输出上限；请缩小查询或使用上游分页参数，不能将片段当作完整结果。"})
	return string(bounded), nil
}

// 不能用字符串前缀判断边界：/notes-other 也以 /notes 开头。
// 先解析符号链接，再用路径相对关系检查，才能挡住 ../ 和指向外部的链接。
// 检查与上游真正打开文件之间仍存在时间间隔；这不是防御恶意并发改写的 OS 沙箱。
func scopedPath(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("Wiki 路径不存在或无法访问")
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径超出当前 Wiki 范围")
	}
	return real, nil
}

// 每次调用都重新解析根目录，避免把已经删除或替换的路径当成初始化时的有效目录。
// 根目录与目标文件必须使用同样的真实路径表示，才能正确比较边界。
func wikiPath(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("请先选择 Wiki 目录")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("Wiki 目录不存在")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("Wiki 路径必须为目录")
	}
	return real, nil
}
