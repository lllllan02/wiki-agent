package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	jsonschema "github.com/eino-contrib/jsonschema"
)

// SchemaValidation 只对当前 Agent 已选择的工具做 best-effort 参数校验。
//
// 它发生在真实工具调用之前、PathPolicy 之前：先确认模型给出的 arguments 至少
// 是工具 schema 描述的基本形状，再让后续中间件处理路径范围、超时等业务规则。
//
// 这个中间件不是工具注册的硬性前提。原因有三个：
//  1. 工具来源可能很多，MCP 或自定义工具不一定都能提供完整 schema。
//  2. schema 是“提前发现低级错误”的辅助防线，不应该因为某个工具 schema 损坏
//     就让整个 Agent 构造失败。
//  3. 真实工具仍然是最终校验者；这里放行不代表参数一定合法，只代表这一层没有
//     足够信息提前拒绝。
//
// 因此这里采用 best-effort 策略：能取到 schema 的工具就校验；取不到 schema 的工具
// 直接放行。这样未来不同 Agent 选择不同工具集时，不会被某个无 schema 工具拖垮。
func SchemaValidation(tools []tool.BaseTool) compose.ToolMiddleware {
	schemas := toolSchemas(context.Background(), tools)
	return compose.ToolMiddleware{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
		return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			// ToolInput.Name 是 EINO 已经解析出的工具名。这里只有命中当前 Agent
			// 工具集中的 schema 才做校验；没有 schema 时保持透明，不改写参数。
			if s := schemas[input.Name]; s != nil {
				if err := validateArguments(input.Arguments, s); err != nil {
					return nil, err
				}
			}
			return next(ctx, input)
		}
	}}
}

// toolSchemas 从当前 Agent 已选择的工具中提取 schema 快照。
//
// 注意这里接收的是 Agent 的工具集，不是全局 MCP 工具目录。这样 Reader、Writer、
// Browser 等未来不同 Agent 可以各自拥有不同工具和不同校验范围。
//
// Info 或 ToJSONSchema 失败时只跳过当前工具。这里不返回 error，是为了保持
// SchemaValidation 的 best-effort 特性：schema 缺失不等于工具不能被调用。
func toolSchemas(ctx context.Context, tools []tool.BaseTool) map[string]*jsonschema.Schema {
	schemas := map[string]*jsonschema.Schema{}
	for _, base := range tools {
		info, err := base.Info(ctx)
		if err != nil {
			continue
		}
		if info.ParamsOneOf == nil {
			continue
		}
		s, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			continue
		}
		schemas[info.Name] = s
	}
	return schemas
}

// validateArguments 校验模型传来的原始 arguments 字符串。
//
// EINO 传给工具中间件的是 JSON 字符串，而不是已经解析好的 map。这里先做两件基础事：
//  1. 确认它是合法 JSON 对象。
//  2. 确认字符串里没有第二个 JSON 值，避免 `{} {}` 这类拼接输入被悄悄接受。
//
// 之后的字段级校验只覆盖当前项目需要的基础 JSON Schema 子集：必填字段、未知字段、
// 基础类型、数组元素、嵌套对象和枚举。更复杂的 oneOf/anyOf/$ref 等规则暂不在这里实现。
func validateArguments(raw string, s *jsonschema.Schema) error {
	args := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&args); err != nil {
			return toolErrorf("invalid_arguments", "参数必须是合法 JSON 对象: %v", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return toolErrorf("invalid_arguments", "参数 JSON 只能包含一个对象")
		}
	}
	return validateObject(args, s, "arguments")
}

// validateObject 校验对象的顶层或嵌套字段。
//
// 这里会拒绝 schema 未声明的字段。这个选择偏严格：模型经常会猜参数名，
// 尽早报出 unknown field 能让模型下一轮修正，而不是把错误参数传给真实工具。
func validateObject(args map[string]any, s *jsonschema.Schema, path string) error {
	for _, required := range s.Required {
		if _, ok := args[required]; !ok {
			return toolErrorf("invalid_arguments", "%s 缺少必填字段 %q", path, required)
		}
	}
	if s.Properties == nil {
		return nil
	}
	for key, value := range args {
		prop, ok := s.Properties.Get(key)
		if !ok {
			return toolErrorf("invalid_arguments", "%s 包含未知字段 %q", path, key)
		}
		if err := validateValue(value, prop, path+"."+key); err != nil {
			return err
		}
	}
	return nil
}

// validateValue 递归检查一个字段值是否符合 schema 的基础类型。
//
// 这不是完整 JSON Schema 引擎，只做足够早期治理使用的轻量检查。
// 复杂约束仍交给真实工具或未来更完整的 schema 组件处理。
func validateValue(value any, s *jsonschema.Schema, path string) error {
	if len(s.Enum) > 0 && !enumContains(s.Enum, value) {
		return toolErrorf("invalid_arguments", "%s 不在允许枚举值中", path)
	}
	switch s.Type {
	case "", "any":
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return toolErrorf("invalid_arguments", "%s 必须是 string", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return toolErrorf("invalid_arguments", "%s 必须是 boolean", path)
		}
	case "number":
		if !isJSONNumber(value) {
			return toolErrorf("invalid_arguments", "%s 必须是 number", path)
		}
	case "integer":
		if !isJSONInteger(value) {
			return toolErrorf("invalid_arguments", "%s 必须是 integer", path)
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return toolErrorf("invalid_arguments", "%s 必须是 array", path)
		}
		if s.Items != nil {
			for i, item := range items {
				if err := validateValue(item, s.Items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return toolErrorf("invalid_arguments", "%s 必须是 object", path)
		}
		return validateObject(object, s, path)
	default:
		return nil
	}
	return nil
}

// enumContains 使用字符串化比较，是为了兼容 json.Number 与 schema enum 中的普通数字值。
// 这里追求的是给模型提供早期反馈，不做精确类型系统推断。
func enumContains(enum []any, value any) bool {
	for _, item := range enum {
		if fmt.Sprint(item) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

// isJSONNumber 接受 json.Number，是因为 decoder.UseNumber 会保留模型传来的数字文本。
// 这样 integer 检查还能判断 1.2 和 1 的差别。
func isJSONNumber(value any) bool {
	switch value.(type) {
	case json.Number, float64, int, int64:
		return true
	default:
		return false
	}
}

func isJSONInteger(value any) bool {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		return err == nil && math.Trunc(f) == f
	case float64:
		return math.Trunc(v) == v
	case int, int64:
		return true
	default:
		return false
	}
}
