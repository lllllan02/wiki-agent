package middleware

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

type Policy struct {
	PathParameters []string
}

// PathPolicy 负责处理“哪些工具参数代表文件路径”这一类调用策略。
//
// 它不是完整权限系统，也不负责决定某个 Agent 能不能使用某个工具；那些事情分别由
// Manager.Select/SelectAvailable 和未来的授权中间件处理。PathPolicy 只做路径参数相关的
// 三件小事：
//  1. 找出当前工具声明过的路径参数，比如 path 或 repo_path。
//  2. 把模型传入的相对路径改写成本轮 Wiki 根目录下的绝对路径。
//  3. 拒绝越过本轮 Wiki 根目录的路径，以及当前阶段不允许读取的非 Markdown 文件。
//
// 为什么需要这个中间件？
// MCP 服务是应用生命周期内共享的，不会为每个 Wiki 目录启动一套进程。模型自然更适合
// 传 "notes/a.md" 这样的相对路径；真实 MCP 工具却需要能直接执行的路径。这个中间件
// 就在工具调用前把“面向模型的相对路径”转换成“面向工具的绝对路径”，同时把目录边界
// 强制住，避免模型或资料内容诱导工具访问 Wiki 外部。
//
// 它通常放在 SchemaValidation 之后、Timeout 之前：
// SchemaValidation 先确认 path 这类参数至少是 string；PathPolicy 再做路径解析和改写；
// Timeout 最后包住真实工具调用。
func PathPolicy(policies map[string]Policy) compose.ToolMiddleware {
	return compose.ToolMiddleware{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
		return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			// policies 只包含当前 Agent 选择的工具。没有路径参数的工具保持透明，
			// 这样同一条中间件链也可以服务搜索、记忆等非文件路径工具。
			policy := policies[input.Name]
			if len(policy.PathParameters) == 0 {
				return next(ctx, input)
			}
			args := map[string]any{}
			if strings.TrimSpace(input.Arguments) != "" {
				if err := json.Unmarshal([]byte(input.Arguments), &args); err != nil {
					return nil, toolErrorf("invalid_arguments", "参数必须是合法 JSON 对象: %v", err)
				}
			}
			root := runcontext.From(ctx).WikiRoot
			if strings.TrimSpace(root) == "" {
				return nil, toolErrorf("permission_denied", "缺少本轮 Wiki 根目录")
			}
			rootAbs, err := filepath.Abs(root)
			if err != nil {
				return nil, toolErrorf("permission_denied", "Wiki 根目录无效")
			}
			for _, name := range policy.PathParameters {
				// 缺失路径参数不在这里报错：是否必填属于 schema 或真实工具的职责。
				// 这里仅处理“出现了且声明为路径”的参数。
				raw, ok := args[name]
				if !ok {
					continue
				}
				pathValue, ok := raw.(string)
				if !ok {
					return nil, toolErrorf("invalid_arguments", "%s 必须是 string", name)
				}
				resolved, err := resolveWikiPath(rootAbs, pathValue)
				if err != nil {
					return nil, err
				}
				args[name] = resolved
			}
			// 中间件不能原地修改 input，避免影响外层中间件观察到的对象。
			// 复制一份 ToolInput，只替换 Arguments，再交给后续中间件和真实工具。
			rewritten, err := json.Marshal(args)
			if err != nil {
				return nil, toolErrorf("invalid_arguments", "参数无法重新编码")
			}
			nextInput := *input
			nextInput.Arguments = string(rewritten)
			return next(ctx, &nextInput)
		}
	}}
}

// resolveWikiPath 把模型传入的路径解析到本轮 Wiki 根目录内。
//
// filepath.Clean 只能规范化路径，不能证明路径在根目录内；真正的边界判断必须用
// filepath.Rel(root, candidate)，再检查结果是否以 .. 逃出根目录。
//
// 这里允许目录路径，因为 list_directory/search_files 这类工具需要访问目录。
// 对文件路径，目前只允许 .md，保持项目当前“本地 Markdown Wiki 助手”的最小安全边界。
func resolveWikiPath(rootAbs, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", toolErrorf("invalid_arguments", "路径不能为空")
	}
	var candidate string
	if filepath.IsAbs(value) {
		candidate = filepath.Clean(value)
	} else {
		candidate = filepath.Join(rootAbs, value)
	}
	rel, err := filepath.Rel(rootAbs, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", toolErrorf("permission_denied", "路径 %q 不在当前 Wiki 内", value)
	}
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate, nil
	}
	if filepath.Ext(candidate) != "" && strings.ToLower(filepath.Ext(candidate)) != ".md" {
		return "", toolErrorf("invalid_arguments", "当前阶段只允许访问 Markdown 文件")
	}
	return candidate, nil
}
