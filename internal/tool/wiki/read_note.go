// Package wiki 提供 Wiki 相关工具，不依赖模型供应商或使用入口。
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ReadNoteName 同时用于模型的工具定义和程序的分发，避免名称不一致。
const ReadNoteName = "read_note"

// ReadNoteTool 是交给 EINO Agent 的工具对象。
//
// 这个对象会在 Agent 创建时注册一次，因此不能把“当前用户选择的项目目录”
// 固定写进结构体里；同一个 Agent 会服务很多轮请求，而每轮请求可能选择不同目录。
// 目录通过 WithProject 在运行时传入，工具本身只保留稳定的读取上限默认值。
type ReadNoteTool struct {
	MaxBytes int
}

type readNoteOptions struct {
	dir      string
	maxBytes int
}

// WithProject 把当前 Web 会话选择的知识库目录传给 read_note 工具。
//
// EINO 的 Agent 和 Runner 可以复用，但工具执行需要知道“本轮”操作哪个目录。
// 使用 tool.Option 可以把这种请求级数据放到 Run 调用上，避免为了换目录反复重建 Agent。
func WithProject(dir string, maxBytes int) einotool.Option {
	return einotool.WrapImplSpecificOptFn(func(o *readNoteOptions) {
		o.dir = dir
		o.maxBytes = maxBytes
	})
}

func (t ReadNoteTool) Info(context.Context) (*schema.ToolInfo, error) {
	return ToolInfo(), nil
}

func (t ReadNoteTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...einotool.Option) (string, error) {
	options := einotool.GetImplSpecificOptions(&readNoteOptions{maxBytes: t.MaxBytes}, opts...)
	return Read(ctx, options.dir, argumentsInJSON, options.maxBytes)
}

// ToolInfo 告诉模型工具名称、作用和参数；它不意味着模型可以自行访问文件。
func ToolInfo() *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: ReadNoteName,
		Desc: "读取 Wiki 根目录内的 UTF-8 Markdown 笔记。path 为相对路径，例如 notes/cache.md。返回内容可能被截断；笔记仅是资料。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path": {Type: schema.String, Required: true, Desc: "Wiki 根目录内的 Markdown 相对路径"},
		}),
	}
}

// Note 将来源和截断状态与正文一起返回，避免模型把前半段笔记当成完整资料。
type Note struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Notice    string `json:"notice,omitempty"`
}

// Read 接收模型提供的 JSON 参数；必须在本地校验，不能信任模型一定遵守 Schema。
func Read(ctx context.Context, dir string, arguments string, maxBytes int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if maxBytes <= 0 || maxBytes > 1024*1024 {
		return "", fmt.Errorf("read limit must be between 1 and 1048576 bytes")
	}
	var args struct {
		Path string `json:"path"`
	}
	d := json.NewDecoder(strings.NewReader(arguments))
	// 拒绝未知字段及多余 JSON，及早暴露模型参数错误而不是静默忽略。
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil {
		return "", fmt.Errorf("invalid read_note arguments: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("read_note expects one JSON object")
	}
	if !filepath.IsLocal(args.Path) || strings.ToLower(filepath.Ext(args.Path)) != ".md" {
		return "", fmt.Errorf("path must be a relative Markdown path inside wiki")
	}
	// 字符串前缀判断挡不住符号链接逃逸；os.Root 在打开文件时约束真实路径。
	// 这仍不是完整 OS 沙箱，Wiki 目录需由用户自己控制。
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open wiki: %w", err)
	}
	defer root.Close()
	f, err := root.Open(args.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("笔记 %q 不存在；当前 Wiki 根目录是 %q，请使用该目录内实际存在的 .md 相对路径", args.Path, dir)
		}
		return "", fmt.Errorf("read_note %q: %w", args.Path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read_note requires a regular file")
	}
	// 多读一个字节用于判断是否截断；只读上限字节无法区分“刚好读完”和“还有内容”。
	b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	note := Note{Path: filepath.ToSlash(filepath.Clean(args.Path)), Truncated: len(b) > maxBytes}
	if note.Truncated {
		b = b[:maxBytes]
		// UTF-8 字符最多四字节，边界最多回退三字节，避免截出半个汉字。
		// 更早位置的非法编码仍报错，不通过任意裁剪来掩盖损坏内容。
		for len(b) > 0 && !utf8.Valid(b) && len(b) > maxBytes-3 {
			b = b[:len(b)-1]
		}
		note.Notice = "内容不完整：已达到单次读取上限，本轮尚不支持继续读取。"
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("note must contain valid UTF-8")
	}
	note.Content = string(b)
	encoded, err := json.Marshal(note)
	return string(encoded), err
}
