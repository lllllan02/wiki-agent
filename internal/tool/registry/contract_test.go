package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestPagedMCPResultContinuation(t *testing.T) {
	var result toolResult
	if err := json.Unmarshal([]byte(pagedFileResult("files__read_file", toolData{Text: "甲\n乙\n\n[showing 2/5 lines]"}, 1024, "note.md", 1, 2)), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "truncated" || !result.Truncated || result.Continuation == nil || result.Continuation.Arguments["offset"] != float64(3) && result.Continuation.Arguments["offset"] != 3 {
		t.Fatalf("分页提示丢失: %+v", result)
	}
	if err := json.Unmarshal([]byte(pagedFileResult("files__read_file", toolData{Text: "丁\n戊\n\n[showing 2/5 lines]"}, 1024, "note.md", 4, 2)), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.Truncated {
		t.Fatalf("末页应完整: %+v", result)
	}
}

func TestDeclaredParameterStructure(t *testing.T) {
	info := &schema.ToolInfo{ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"path": {Type: schema.String, Required: true},
		"head": {Type: schema.Integer},
	})}
	for _, args := range []map[string]any{{}, {"path": 3.0}, {"path": "note.md", "head": 1.5}} {
		if err := validateArguments(info, args); err == nil {
			t.Fatalf("无效参数通过检查: %+v", args)
		}
	}
	if err := validateArguments(info, map[string]any{"path": "note.md", "head": 2.0}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPTextEmptyAndTruncation(t *testing.T) {
	if got := boundedResult("fixture__search", mcpData(`{"content":[]}`), 12, nil); !strings.Contains(got, `"status":"empty"`) {
		t.Fatalf("空结果未区分: %s", got)
	}
	var result toolResult
	if err := json.Unmarshal([]byte(boundedResult("fixture__read", toolData{Text: "一二三四五"}, 7, &continuation{Hint: "继续"})), &result); err != nil || result.Data.Text != "一二" || result.Status != "truncated" {
		t.Fatalf("截断破坏 UTF-8: %+v %v", result, err)
	}
}

func TestStructuredMCPFieldsSurviveContract(t *testing.T) {
	data := mcpData(`{"content":[{"type":"text","text":"summary"}],"structuredContent":{"count":2,"items":["a","b"]}}`)
	output := boundedResult("fixture__query", data, 1024, nil)
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Text       string          `json:"text"`
			Structured json.RawMessage `json:"structured"`
		} `json:"data"`
		Error        *toolError    `json:"error"`
		Source       string        `json:"source"`
		Truncated    bool          `json:"truncated"`
		Continuation *continuation `json:"continuation"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil || result.Status != "ok" || result.Data.Text != "summary" || !strings.Contains(string(result.Data.Structured), `"count":2`) || result.Error != nil || result.Continuation != nil {
		t.Fatalf("MCP 结构化数据丢失: %s %v", output, err)
	}
	if !strings.Contains(output, `"error":null`) || !strings.Contains(output, `"continuation":null`) {
		t.Fatalf("固定字段缺失: %s", output)
	}
}

func TestSearchNoMatchIsEmpty(t *testing.T) {
	tool := &executionTool{
		upstream: &recordingTool{result: `{"content":[{"type":"text","text":"No matches found."}]}`},
		info:     &schema.ToolInfo{Name: "ripgrep__search"},
		timeout:  1e9,
		maxBytes: 1024,
	}
	output, err := tool.InvokableRun(context.Background(), `{"pattern":"absent"}`)
	if err != nil || !strings.Contains(output, `"status":"empty"`) {
		t.Fatalf("无匹配未区分为空结果: %s %v", output, err)
	}
}
