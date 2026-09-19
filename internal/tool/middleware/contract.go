package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/cloudwego/eino/compose"
)

// callError 是工具治理链内部的错误类型，不是项目级公共错误体系。
//
// SchemaValidation、PathPolicy 这类前置中间件需要告诉最外层的 ContractMiddleware：
// “这个失败应该以什么稳定 code 交给模型”。直接返回普通 error 也可以，
// 但最外层只能把它归成 tool_failure，模型就无法区分参数错误、权限错误或临时故障。
//
// 这个类型故意保持未导出：它只服务于工具调用结果契约。Agent 其它链路如果未来
// 需要统一错误体系，应另行设计，不能把这里的工具结果 code 扩散成全项目错误模型。
type callError struct {
	code    string
	message string
}

func (e *callError) Error() string { return e.message }

func toolErrorf(code, format string, args ...any) error {
	return &callError{code: code, message: fmt.Sprintf(format, args...)}
}

// Contract 是交给模型看的工具结果外壳。
//
// EINO 和 MCP 工具本身只要求工具返回一段字符串；不同工具可能返回纯文本、JSON、
// 错误字符串或空结果。对模型来说，这些形态不好稳定解释：空字符串到底是没找到、
// 执行失败，还是工具确实返回空？ContractMiddleware 把这些情况包成统一结构：
// status 表示结果状态，成功时固定为 ok，失败时就是 invalid_arguments、permission_denied、
// timeout 这类可理解的错误码；result 保存工具原始文本；error 保存失败说明。
//
// 注意：这里不做摘要、截断、重试或上下文预算管理。这个中间件只负责“把一次工具调用
// 的最终状态说清楚”，结果长度治理会放到后续专门组件里。
type Contract struct {
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ContractMiddleware 是工具治理链的最外层中间件。
//
// 它既有“后置”职责，也有“兜底”职责：
//  1. 调用 next(ctx, input) 之前不改参数，不做权限判断，让更内层的中间件负责。
//  2. next 成功返回后，把工具原始 output.Result 包装成 Contract JSON。
//  3. next 返回错误时，把错误转换成 Contract JSON，而不是让错误继续冒泡中断整轮 Agent。
//  4. next 或更内层中间件 panic 时，用 defer 兜住并转换成 tool_failure。
//
// 为什么错误要变成工具结果，而不是直接返回 error？
// 在 ReAct 流程里，参数错误、权限拒绝、超时等通常是模型可以理解并修正的反馈。
// 如果这些错误直接冒泡到 Runner，整轮对话会失败，模型没有机会改参数或换工具。
// 所以这个中间件把“工具调用失败”表达成一条工具消息，交回给模型继续推理。
//
// 为什么它必须注册在最外层？
// EINO 的中间件按注册顺序进入、反序退出。ContractMiddleware 放在第一位时，
// SchemaValidation、PathPolicy、Timeout 以及真实工具的错误都会被它统一包装。
// 如果它放到更内层，外层中间件产生的错误就绕过了契约。
//
// 当前只实现 Invokable 分支，因为本项目开放的 MCP 工具都是普通调用工具。
// 后续如果开放 StreamableTool，应新增 Streamable 分支，并在流读取结束或出错时
// 生成同样的契约，不能假设这个 Invokable 包装会自动覆盖流式工具。
func ContractMiddleware() compose.ToolMiddleware {
	return compose.ToolMiddleware{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
		return func(ctx context.Context, input *compose.ToolInput) (output *compose.ToolOutput, err error) {
			// defer 覆盖 next 的整个执行过程，包括内层中间件和真实工具。
			// 这里把 panic/error 都改写成工具结果，并清空 err，目的是让 EINO
			// 把这次失败作为 ToolMessage 回填给模型，而不是终止整轮 Runner。
			defer func() {
				if recovered := recover(); recovered != nil {
					output = contractOutput(input.Name, "", fmt.Errorf("panic: %v", recovered))
					err = nil
					// 目前没有结构化日志系统；保留 Stack 调用位置，后续接入事件日志时
					// 可以在这里记录 panic 栈。不要把 stack 放进工具结果，避免泄露本地路径。
					_ = debug.Stack()
				}
				if _, interrupted := compose.IsInterruptRerunError(err); interrupted {
					return // 中断是控制信号，必须交给 Eino 保存检查点。
				}
				if err != nil {
					output = contractOutput(input.Name, "", err)
					err = nil
				}
			}()
			// next 会依次进入后续中间件，最后到达真实工具。ContractMiddleware
			// 不提前解释参数，因为参数 schema、路径策略和 timeout 分别属于自己的中间件。
			output, err = next(ctx, input)
			if err != nil {
				return nil, err
			}
			// 工具返回 nil output 并不等于框架错误；这里把它视为成功但文本为空。
			// 这样模型能看到 status=ok/result=""，而不是因为 nil 指针导致运行失败。
			result := ""
			if output != nil {
				result = output.Result
			}
			return contractOutput(input.Name, result, nil), nil
		}
	}}
}

// contractOutput 只把一次工具调用的“最终状态”编码成字符串。
//
// EINO ToolNode 的输出类型是 compose.ToolOutput{Result string}，所以契约最终也必须是字符串。
// 这也是为什么 Contract 是 JSON：它既能保持工具结果仍是一条普通工具消息，又能给模型
// 一个稳定可解析的结构。
func contractOutput(toolName, text string, err error) *compose.ToolOutput {
	contract := Contract{
		Status: "ok",
		Result: text,
	}
	if err != nil {
		code, message := classifyToolError(err)
		contract.Status = code
		contract.Result = ""
		contract.Error = message
	}
	result, marshalErr := json.Marshal(contract)
	if marshalErr != nil {
		// 理论上 Contract 只包含可 JSON 化字段，不应失败。这里仍保留兜底，
		// 防止未来调整字段时，让工具调用链直接崩掉。toolName 只用于兜底信息，
		// 正常调用关联仍由 EINO 的 ToolCall ID 负责。
		return &compose.ToolOutput{Result: fmt.Sprintf(`{"status":"tool_failure","error":%q}`, fmt.Sprintf("%s: %s", toolName, marshalErr.Error()))}
	}
	return &compose.ToolOutput{Result: string(result)}
}

// classifyToolError 把 Go error 压缩成模型可理解的少量错误 code。
//
// 这里刻意不保留 Go 类型层级，也不把本地错误原样暴露成复杂结构。
// 模型真正需要的是下一步策略：参数错了就修参数，权限拒绝就换范围，
// 超时就缩小请求或稍后重试，未知工具失败才交给用户。
func classifyToolError(err error) (string, string) {
	if typed, ok := err.(*callError); ok {
		return typed.code, typed.message
	}
	if context.DeadlineExceeded == err || strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") {
		return "timeout", "工具调用超时"
	}
	if context.Canceled == err {
		return "temporary_failure", "工具调用被取消"
	}
	return "tool_failure", err.Error()
}
