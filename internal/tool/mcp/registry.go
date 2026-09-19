// Package mcp 负责接入现成 MCP；不实现文件、搜索或浏览器工具本身。
package mcp

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
}

type Registry struct {
	Servers []Server `yaml:"servers"`
	baseDir string
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Load 读取共享 MCP 服务清单，不启动连接。
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
		if strings.Contains(s.Command, "${WIKI_ROOT}") || strings.Contains(s.URL, "${WIKI_ROOT}") {
			return nil, fmt.Errorf("MCP %s: 共享服务不能使用 ${WIKI_ROOT}；目录应通过工具参数传入", s.Name)
		}
		for _, value := range s.Args {
			if strings.Contains(value, "${WIKI_ROOT}") {
				return nil, fmt.Errorf("MCP %s: 共享服务不能使用 ${WIKI_ROOT}；目录应通过工具参数传入", s.Name)
			}
		}
		for _, value := range s.Env {
			if strings.Contains(value, "${WIKI_ROOT}") {
				return nil, fmt.Errorf("MCP %s: 共享服务不能使用 ${WIKI_ROOT}；目录应通过工具参数传入", s.Name)
			}
		}
		for _, value := range s.Headers {
			if strings.Contains(value, "${WIKI_ROOT}") {
				return nil, fmt.Errorf("MCP %s: 共享服务不能使用 ${WIKI_ROOT}；目录应通过工具参数传入", s.Name)
			}
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

	}
	return r, nil
}

// ConnectionSet 由 Tool Manager 在服务生命周期内复用；关闭时取消并释放传输。
type ConnectionSet struct {
	Tools    []tool.BaseTool
	Policies map[string]ToolPolicy
	clients  []*client.Client
	cancel   context.CancelFunc
}

// ToolPolicy 保存来自 MCP 注册表的业务约束；工具调用治理层会在实际调用前消费它。
type ToolPolicy struct {
	PathParameters []string
}

func (s *ConnectionSet) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	for i := len(s.clients) - 1; i >= 0; i-- {
		_ = s.clients[i].Close()
	}
	s.clients = nil
}

// Open 使用 MCP Go 客户端与 EINO GetTools，仅负责连接、白名单和工具命名。
func (r *Registry) Open(ctx context.Context, cfg config.MCP) (_ *ConnectionSet, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	connections := &ConnectionSet{cancel: cancel, Policies: map[string]ToolPolicy{}}
	defer func() {
		if err != nil {
			connections.Close()
		}
	}()
	for _, server := range r.Servers {
		if !server.Enabled {
			continue
		}
		if cfg.Timeout <= 0 {
			return nil, fmt.Errorf("MCP 超时必须大于 0")
		}
		expand := strings.NewReplacer("${PROJECT_ROOT}", r.baseDir).Replace
		var cli *client.Client
		if server.Name == "memory" {
			if path := server.Env["MEMORY_FILE_PATH"]; path != "" {
				if err := os.MkdirAll(filepath.Dir(expand(path)), 0700); err != nil {
					return nil, fmt.Errorf("MCP memory: 无法创建存储目录")
				}
			}
		}
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
		connections.clients = append(connections.clients, cli)
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
			prefixedName := copyInfo.Name
			connections.Tools = append(connections.Tools, &namedTool{upstream: invokable, info: &copyInfo})
			connections.Policies[prefixedName] = ToolPolicy{PathParameters: append([]string(nil), server.PathParameters...)}
		}
		for _, name := range server.Tools {
			if !found[name] {
				return nil, fmt.Errorf("MCP %s: 配置的工具 %s 未由服务器提供，请核对版本/白名单", server.Name, name)
			}
		}
	}
	return connections, nil
}

type namedTool struct {
	upstream tool.InvokableTool
	info     *schema.ToolInfo
}

func (t *namedTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *namedTool) InvokableRun(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
	return t.upstream.InvokableRun(ctx, arguments, opts...)
}
