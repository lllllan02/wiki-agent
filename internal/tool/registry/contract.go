package registry

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

// toolResult 是所有交给模型的固定 JSON 契约；无值字段显式返回 null。
type toolResult struct {
	Status       string        `json:"status"`
	Data         toolData      `json:"data"`
	Error        *toolError    `json:"error"`
	Source       string        `json:"source"`
	Truncated    bool          `json:"truncated"`
	Continuation *continuation `json:"continuation"`
}

type toolData struct {
	Text       string          `json:"text"`
	Structured json.RawMessage `json:"structured"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type continuation struct {
	Tool      string         `json:"tool,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Hint      string         `json:"hint,omitempty"`
}

func resultJSON(r toolResult) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func errorResult(source, message string) string {
	return errorResultCode(source, "tool_failed", message)
}

func errorResultCode(source, code, message string) string {
	return resultJSON(toolResult{Status: "error", Source: source, Error: &toolError{Code: code, Message: message}})
}

func boundedResult(source string, data toolData, maxBytes int, next *continuation) string {
	r := toolResult{Status: "ok", Data: data, Source: source}
	if len(data.Text)+len(data.Structured) > maxBytes {
		r.Truncated = true
		r.Status = "truncated"
		prefix := data.Text
		if prefix == "" {
			prefix = string(data.Structured)
		}
		if len(prefix) > maxBytes {
			prefix = prefix[:maxBytes]
		}
		for !utf8.ValidString(prefix) {
			prefix = prefix[:len(prefix)-1]
		}
		r.Data = toolData{Text: prefix}
		r.Continuation = next
	} else if strings.TrimSpace(data.Text) == "" && len(data.Structured) == 0 {
		r.Status = "empty"
	}
	return resultJSON(r)
}

// 先检查 MCP 公布的顶层参数结构；路径范围和搜索限额在执行层做业务校验。
// 复杂的组合 schema 仍由上游 MCP 校验，不能把此检查当作完整 JSON Schema 实现。
func validateArguments(info *schema.ToolInfo, args map[string]any) error {
	if info == nil || info.ParamsOneOf == nil {
		return nil
	}
	s, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		return fmt.Errorf("工具参数定义不可用")
	}
	if s == nil {
		return nil
	}
	for _, name := range s.Required {
		if _, ok := args[name]; !ok {
			return fmt.Errorf("缺少必填参数 %s", name)
		}
	}
	if s.Properties == nil {
		return nil
	}
	for key, value := range args {
		field, ok := s.Properties.Get(key)
		if !ok || field == nil {
			continue
		}
		valid := true
		switch field.Type {
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "number":
			_, valid = value.(float64)
		case "integer":
			n, ok := value.(float64)
			valid = ok && n == float64(int64(n))
		case "array":
			_, valid = value.([]any)
		case "object":
			_, valid = value.(map[string]any)
		}
		if !valid {
			return fmt.Errorf("参数 %s 类型不符合工具定义", key)
		}
		if len(field.Enum) > 0 {
			allowed := false
			for _, option := range field.Enum {
				if reflect.DeepEqual(option, value) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("参数 %s 不在允许值中", key)
			}
		}
		if str, ok := value.(string); ok {
			length := uint64(utf8.RuneCountInString(str))
			if field.MinLength != nil && length < *field.MinLength || field.MaxLength != nil && length > *field.MaxLength {
				return fmt.Errorf("参数 %s 长度超出允许范围", key)
			}
		}
		if number, ok := value.(float64); ok {
			if math.IsNaN(number) || math.IsInf(number, 0) || below(number, field.Minimum, false) || below(number, field.ExclusiveMinimum, true) || above(number, field.Maximum, false) || above(number, field.ExclusiveMaximum, true) {
				return fmt.Errorf("参数 %s 超出允许范围", key)
			}
		}
	}
	return nil
}

func below(value float64, bound json.Number, exclusive bool) bool {
	if bound == "" {
		return false
	}
	n, err := bound.Float64()
	return err == nil && (value < n || exclusive && value == n)
}

func above(value float64, bound json.Number, exclusive bool) bool {
	if bound == "" {
		return false
	}
	n, err := bound.Float64()
	return err == nil && (value > n || exclusive && value == n)
}

// EINO MCP 适配器把 CallToolResult 序列化为 JSON；同时保留文本和 structuredContent。
func mcpData(raw string) toolData {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	if json.Unmarshal([]byte(raw), &result) != nil || result.Content == nil && len(result.Structured) == 0 {
		return toolData{Text: raw}
	}
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}
	if len(result.Content) > 0 && len(parts) == 0 && len(result.Structured) == 0 {
		return toolData{Text: raw}
	}
	data := toolData{Text: strings.Join(parts, "\n")}
	if len(result.Structured) > 0 && string(result.Structured) != "null" {
		var fields map[string]json.RawMessage
		duplicate := false
		if json.Unmarshal(result.Structured, &fields) == nil && len(fields) == 1 {
			var content string
			duplicate = json.Unmarshal(fields["content"], &content) == nil && content == data.Text
		}
		if !duplicate {
			data.Structured = result.Structured
		}
	}
	return data
}
