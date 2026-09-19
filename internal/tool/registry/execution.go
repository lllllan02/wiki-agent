package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	source := t.info.Name
	// 只有声明了路径参数的工具才依赖 Wiki；网络搜索等工具不能被无关的目录条件阻挡。
	var root string
	if len(t.pathParameters) > 0 {
		var err error
		root, err = wikiPath(runcontext.From(ctx).WikiRoot)
		if err != nil {
			return errorResultCode(source, "invalid_scope", err.Error()), nil
		}
	}
	// schema 和提示词不能代替程序校验：模型输出仍可能是 null、数组或错误类型。
	// 在本轮局部 map 上改写参数，不能改共享工具对象，也不能直接转发未检查的原始 JSON。
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return errorResultCode(source, "invalid_arguments", "工具参数必须是 JSON 对象"), nil
	}
	if err := validateArguments(t.info, args); err != nil {
		return errorResultCode(source, "invalid_arguments", err.Error()), nil
	}
	inputPath, _ := args["path"].(string)
	// 参数名来自受信任的部署配置。路径由“应用选定目录 + 模型相对路径”确定，
	// 不接受模型自行指定一个新的根目录；转换为绝对路径也避免依赖 MCP 进程工作目录。
	for _, key := range t.pathParameters {
		path, ok := args[key].(string)
		if !ok || strings.TrimSpace(path) == "" {
			return errorResultCode(source, "invalid_arguments", fmt.Sprintf("%s 必须是 Wiki 内的路径", key)), nil
		}
		resolved, err := scopedPath(root, path)
		if err != nil {
			return errorResultCode(source, "invalid_scope", err.Error()), nil
		}
		args[key] = resolved
	}
	// 下面是当前教学项目的两条具体产品规则，不是假装通用的插件规则引擎。
	// 上游工具支持更多文件类型，但本阶段只承诺读取 Markdown，因而在统一出口收紧能力。
	if t.info.Name == "filesystem__read_text_file" {
		path, _ := args["path"].(string)
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return errorResultCode(source, "invalid_arguments", "当前 Wiki 只开放 Markdown 正文读取"), nil
		}
		if args["head"] != nil && args["tail"] != nil {
			return errorResultCode(source, "invalid_arguments", "head 与 tail 不能同时使用"), nil
		}
		for _, key := range []string{"head", "tail"} {
			if value, exists := args[key]; exists {
				n, ok := value.(float64)
				if !ok || n < 1 || n > 1000 || n != float64(int(n)) {
					return errorResultCode(source, "invalid_arguments", key+" 必须是 1 到 1000 的整数"), nil
				}
			}
		}
	}
	if source == "files__read_file" {
		path, _ := args["path"].(string)
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return errorResultCode(source, "invalid_arguments", "当前 Wiki 只开放 Markdown 正文读取"), nil
		}
		for _, key := range []string{"offset", "limit"} {
			n, ok := args[key].(float64)
			if !ok || n < 1 || n > 1e9 || n != float64(int(n)) || key == "limit" && n > 200 {
				return errorResultCode(source, "invalid_arguments", "分页读取必须指定 offset（从 1 开始）和 limit（1 到 200 行）"), nil
			}
		}
		if args["head"] != nil || args["tail"] != nil {
			return errorResultCode(source, "invalid_arguments", "分页读取只使用 offset 和 limit"), nil
		}
	}
	if t.info.Name == "ripgrep__search" {
		pattern, ok := args["pattern"].(string)
		if !ok || strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "-") {
			return errorResultCode(source, "invalid_arguments", "搜索模式不能为空或以 - 开头"), nil
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
				return errorResultCode(source, "invalid_arguments", fmt.Sprintf("%s 必须是允许范围内的整数，最大 %g", key, limit)), nil
			}
		}
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return errorResult(source, "工具参数编码失败"), nil
	}
	// 超时从请求 context 派生：保留会话信息，并同时响应用户取消与工具超时。
	// 只取消这次调用，不能取消 Manager 使用的应用 context，否则会关闭其他 Agent 的连接。
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	result, err := t.upstream.InvokableRun(callCtx, string(payload), opts...)
	if err != nil {
		if callCtx.Err() != nil {
			return errorResultCode(source, "cancelled_or_timeout", "工具调用取消或超时"), nil
		}
		// 外部错误可能回显认证信息或资料；只透传我们自己生成的工具名和通用提示。
		return errorResult(source, "工具调用失败，请检查参数或服务状态"), nil
	}
	next := &continuation{Hint: "缩小查询范围后重试"}
	if source == "filesystem__read_text_file" {
		next = &continuation{Tool: "files__read_file", Arguments: map[string]any{"path": inputPath, "offset": 1, "limit": 50}}
	}
	data := mcpData(result)
	if source == "files__read_file" {
		return pagedFileResult(source, data, t.maxBytes, inputPath, int(args["offset"].(float64)), int(args["limit"].(float64))), nil
	}
	if (source == "ripgrep__search" || source == "filesystem__search_files") && (data.Text == "No matches found" || data.Text == "No matches found.") {
		data = toolData{}
	}
	return boundedResult(source, data, t.maxBytes, next), nil
}

var pageNote = regexp.MustCompile(`\[showing (\d+)/(\d+) lines\]$`)

func pagedFileResult(source string, data toolData, maxBytes int, path string, offset, limit int) string {
	if len(data.Text)+len(data.Structured) > maxBytes {
		if limit == 1 {
			return boundedResult(source, data, maxBytes, &continuation{Hint: "单行超过输出上限，无法用行窗口完整读取"})
		}
		return boundedResult(source, data, maxBytes, &continuation{Tool: source, Arguments: map[string]any{"path": path, "offset": offset, "limit": max(1, limit/2)}})
	}
	r := toolResult{Status: "ok", Data: data, Source: source}
	if strings.TrimSpace(data.Text) == "" && len(data.Structured) == 0 {
		r.Status = "empty"
		return resultJSON(r)
	}
	if match := pageNote.FindStringSubmatch(strings.TrimSpace(data.Text)); len(match) == 3 {
		count, _ := strconv.Atoi(match[1])
		total, _ := strconv.Atoi(match[2])
		if next := offset + count; next <= total {
			r.Status = "truncated"
			r.Truncated = true
			r.Continuation = &continuation{Tool: source, Arguments: map[string]any{"path": path, "offset": next, "limit": limit}}
		}
	}
	return resultJSON(r)
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
