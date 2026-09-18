// Package mcp 负责接入现成 MCP；不实现文件、搜索或浏览器工具本身。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	einomcp "github.com/cloudwego/eino-ext/components/tool/mcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"go.yaml.in/yaml/v3"
)

// Server 是部署配置，不接受模型生成的启动命令、地址或白名单。
type Server struct {
	Name           string            `yaml:"name"`
	Enabled        bool              `yaml:"enabled"`
	Transport      string            `yaml:"transport"`
	Command        string            `yaml:"command"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	URL            string            `yaml:"url"`
	Headers        map[string]string `yaml:"headers"`
	Tools          []string          `yaml:"tools"`
	PathParameters []string          `yaml:"path_parameters"`
	BoundWiki      string            `yaml:"bound_wiki"` // 远程 Wiki 的本地对应目录；不是远程目录自动切换协议。
}

type Registry struct {
	Servers []Server `yaml:"servers"`
	baseDir string
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Load 只注册清单，不提前连接。知识库由 Web 会话选择，因此连接按运行隔离。
func Load(path string) (*Registry, error) {
	r := &Registry{}
	if path == "" {
		return r, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("读取 MCP 注册表: %w", err)
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err := d.Decode(r); err != nil {
		// 不回显 YAML 内容，注册表可能包含认证头。
		return nil, fmt.Errorf("MCP 注册表格式错误或包含未知字段")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("MCP 注册表必须只有一个 YAML 文档")
	}
	r.baseDir = filepath.Dir(abs)
	seen := map[string]bool{}
	for _, s := range r.Servers {
		if !validName.MatchString(s.Name) || seen[s.Name] {
			return nil, fmt.Errorf("MCP 名称无效或重复: %q", s.Name)
		}
		seen[s.Name] = true
		if s.Transport != "stdio" && s.Transport != "streamable_http" {
			return nil, fmt.Errorf("MCP %s: transport 必须为 stdio 或 streamable_http", s.Name)
		}
		if len(s.Tools) == 0 {
			return nil, fmt.Errorf("MCP %s: 必须显式配置 tools 白名单，空列表不代表全部开放", s.Name)
		}
		toolSeen := map[string]bool{}
		for _, name := range s.Tools {
			if name == "" || toolSeen[name] {
				return nil, fmt.Errorf("MCP %s: 工具名为空或重复", s.Name)
			}
			toolSeen[name] = true
		}
		if !s.Enabled {
			continue
		}
		if s.Transport == "stdio" && s.Command == "" {
			return nil, fmt.Errorf("MCP %s: 缺少 command", s.Name)
		}
		if s.Transport == "streamable_http" {
			u, err := url.Parse(s.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
				return nil, fmt.Errorf("MCP %s: 请配置有效的 HTTP 服务地址", s.Name)
			}
		}
		if s.Name == "obsidian" && s.BoundWiki == "" {
			return nil, fmt.Errorf("MCP obsidian: 请配置 bound_wiki，绑定远程 vault 对应的本地目录")
		}
	}
	return r, nil
}

// Session 只属于本轮运行。关闭时先取消进程，再关闭传输，避免退出阻塞和串库。
type Session struct {
	Tools   []tool.BaseTool
	clients []*client.Client
	cancel  context.CancelFunc
}

func (s *Session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	for i := len(s.clients) - 1; i >= 0; i-- {
		_ = s.clients[i].Close()
	}
	s.clients = nil
}

// Open 复用 MCP Go 客户端与 EINO GetTools，仅添加命名、白名单和本项目边界。
func (r *Registry) Open(ctx context.Context, root string, cfg config.MCP) (_ *Session, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	session := &Session{cancel: cancel}
	defer func() {
		if err != nil {
			session.Close()
		}
	}()
	for _, server := range r.Servers {
		if !server.Enabled {
			continue
		}
		if cfg.Timeout <= 0 || cfg.MaxBytes <= 0 {
			return nil, fmt.Errorf("MCP 超时和输出上限必须大于 0")
		}
		if strings.TrimSpace(root) == "" {
			return nil, fmt.Errorf("请先选择 Wiki 目录")
		}
		wikiRoot, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		wikiRoot, err = filepath.EvalSymlinks(wikiRoot)
		if err != nil {
			return nil, fmt.Errorf("MCP 知识库目录不存在")
		}
		info, err := os.Stat(wikiRoot)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("MCP 知识库必须为目录")
		}
		expand := strings.NewReplacer("${WIKI_ROOT}", wikiRoot, "${PROJECT_ROOT}", r.baseDir).Replace
		if server.BoundWiki != "" {
			bound, err := filepath.EvalSymlinks(expand(server.BoundWiki))
			if err != nil || bound != wikiRoot {
				return nil, fmt.Errorf("MCP %s 绑定的知识库与当前会话不同", server.Name)
			}
		}
		var cli *client.Client
		if server.Transport == "stdio" {
			args := make([]string, len(server.Args))
			for i, v := range server.Args {
				args[i] = expand(v)
			}
			var env []string
			for k, v := range server.Env {
				env = append(env, k+"="+expand(v))
			}
			cli = client.NewClient(transport.NewStdio(expand(server.Command), env, args...))
		} else {
			cli, err = client.NewStreamableHttpClient(server.URL, transport.WithHTTPHeaders(server.Headers), transport.WithHTTPTimeout(cfg.Timeout))
			if err != nil {
				return nil, fmt.Errorf("MCP %s: 无法创建 HTTP 连接", server.Name)
			}
		}
		session.clients = append(session.clients, cli)
		if err := cli.Start(runCtx); err != nil {
			return nil, fmt.Errorf("MCP %s: 启动失败，请检查命令、运行依赖或服务地址", server.Name)
		}
		// 外部进程日志不进入模型或页面，避免上游回显凭据/整份资料。
		if stderr, ok := client.GetStderr(cli); ok {
			go func() { _, _ = io.Copy(io.Discard, stderr) }()
		}
		initCtx, finish := context.WithTimeout(runCtx, cfg.Timeout)
		request := protocol.InitializeRequest{}
		request.Params.ProtocolVersion = protocol.LATEST_PROTOCOL_VERSION
		request.Params.ClientInfo = protocol.Implementation{Name: "wiki-agent", Version: "0.1.0"}
		_, err = cli.Initialize(initCtx, request)
		finish()
		if err != nil {
			return nil, fmt.Errorf("MCP %s: 握手失败，请检查服务版本、认证或超时", server.Name)
		}
		discoverCtx, finish := context.WithTimeout(runCtx, cfg.Timeout)
		available, err := einomcp.GetTools(discoverCtx, &einomcp.Config{Cli: cli, ToolNameList: server.Tools})
		finish()
		if err != nil {
			return nil, fmt.Errorf("MCP %s: 工具发现失败", server.Name)
		}
		found := map[string]bool{}
		for _, base := range available {
			info, err := base.Info(runCtx)
			if err != nil {
				return nil, fmt.Errorf("MCP %s: 无法读取工具定义", server.Name)
			}
			invokable, ok := base.(tool.InvokableTool)
			if !ok {
				return nil, fmt.Errorf("MCP %s: 工具 %s 不支持调用", server.Name, info.Name)
			}
			found[info.Name] = true
			copyInfo := *info
			copyInfo.Name = server.Name + "__" + info.Name
			if len(copyInfo.Name) > 64 || !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(copyInfo.Name) {
				return nil, fmt.Errorf("MCP %s: 工具名不兼容模型接口", server.Name)
			}
			if len(server.PathParameters) > 0 {
				copyInfo.Desc += " 路径可以使用当前 Wiki 内的相对路径；根目录用 .，禁止访问 Wiki 外部。"
			}
			session.Tools = append(session.Tools, &scopedTool{upstream: invokable, info: &copyInfo, server: server, root: wikiRoot, cfg: cfg, originalName: info.Name})
		}
		for _, name := range server.Tools {
			if !found[name] {
				return nil, fmt.Errorf("MCP %s: 配置的工具 %s 未由服务器提供，请核对版本/白名单", server.Name, name)
			}
		}
	}
	return session, nil
}

type scopedTool struct {
	upstream     tool.InvokableTool
	info         *schema.ToolInfo
	server       Server
	root         string
	cfg          config.MCP
	originalName string
}

func (t *scopedTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *scopedTool) InvokableRun(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return "", fmt.Errorf("MCP 参数必须是 JSON 对象")
	}
	for _, key := range t.server.PathParameters {
		path, ok := args[key].(string)
		if !ok || path == "" {
			return "", fmt.Errorf("%s 必须是 Wiki 内的路径", key)
		}
		resolved, err := scopedPath(t.root, path)
		if err != nil {
			return "", err
		}
		args[key] = resolved
	}
	if t.server.Name == "filesystem" && t.originalName == "read_text_file" {
		path, _ := args["path"].(string)
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return "", fmt.Errorf("当前 Wiki 只开放 Markdown 正文读取")
		}
	}
	if t.server.Name == "ripgrep" {
		// 上游 search 会构造 rg 参数；拒绝以 - 开头的模式，避免被解释成命令选项。
		pattern, ok := args["pattern"].(string)
		if !ok || strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "-") {
			return "", fmt.Errorf("搜索模式不能为空或以 - 开头")
		}
		args["filePattern"], args["useColors"] = "*.md", false
		for key, limit := range map[string]float64{"maxResults": 100, "context": 5} {
			v, exists := args[key]
			if !exists {
				if key == "maxResults" {
					args[key] = limit
				}
				continue
			}
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
	callCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	result, err := t.upstream.InvokableRun(callCtx, string(payload), opts...)
	if err != nil {
		// 网络错误和服务端错误可能携带认证信息；不直接返回原始错误。
		if callCtx.Err() != nil {
			return "", fmt.Errorf("MCP %s 调用取消或超时: %w", t.info.Name, callCtx.Err())
		}
		return "", fmt.Errorf("MCP %s 调用失败，请检查参数或服务状态", t.info.Name)
	}
	if len(result) <= t.cfg.MaxBytes {
		return result, nil
	}
	result = result[:t.cfg.MaxBytes]
	for !utf8.ValidString(result) && len(result) > 0 {
		result = result[:len(result)-1]
	}
	bounded, _ := json.Marshal(map[string]any{"truncated": true, "content_prefix": result, "notice": "MCP 结果超过输出上限；请缩小查询或使用上游分页参数，不能将片段当作完整结果。"})
	return string(bounded), nil
}

// 路径校验是调用前门控。根目录仍应由用户控制，不能把它声称为对恶意并发修改的 OS 沙箱。
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
