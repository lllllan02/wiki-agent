// Package registry 连接 MCP 工具、复用连接，并在每次执行时施加 Wiki 范围约束。
// mcp 包负责协议与工具发现；EINO 负责按名称选择工具。这里不再维护另一份调用分发表。
package registry

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/lllllan02/wiki-agent/internal/config"
	mcptools "github.com/lllllan02/wiki-agent/internal/tool/mcp"
)

// Manager 持有应用生命周期内共享的工具与 MCP 连接。
// 它不保存当前 Wiki；不同请求的目录由各自的调用 context 提供。
type Manager struct {
	key         managerKey
	tools       []tool.BaseTool
	connections *mcptools.ConnectionSet
	closed      chan struct{}
	once        sync.Once
}

// 共享键同时包含生命周期和配置，避免不同应用实例或不同 MCP 配置误用连接。
// Done 通道代表可取消的应用 context；调用方不能传一次 HTTP 请求的 context。
type managerKey struct {
	lifetime <-chan struct{}
	config   config.MCP
}

// 只在成功初始化后放入表中；失败不缓存，修正配置或服务后可以重试。
var sharedManagers = struct {
	sync.Mutex
	items map[managerKey]*Manager
}{items: make(map[managerKey]*Manager)}

// LoadMCP 在 Agent 构造时调用，为同一应用生命周期及配置只创建一组连接。
// 连接属于整个应用，单次模型运行结束时不关闭它。
func LoadMCP(lifetime context.Context, cfg config.MCP) (_ *Manager, err error) {
	if strings.TrimSpace(cfg.RegistryFile) == "" {
		return nil, fmt.Errorf("请配置 mcp.registry_file，并安装已启用的 MCP 服务")
	}
	cfg.RegistryFile, err = filepath.Abs(cfg.RegistryFile)
	if err != nil {
		return nil, err
	}
	// 把相对路径转为绝对路径后作为键的一部分，等价路径才会命中同一实例。
	key := managerKey{lifetime: lifetime.Done(), config: cfg}
	// 锁覆盖查表、握手和发布：并发构造 Agent 时不会重复启动 MCP 服务。
	// 这把锁只用于初始化与关闭，不包住实际工具调用。
	sharedManagers.Lock()
	defer sharedManagers.Unlock()
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	if manager := sharedManagers.items[key]; manager != nil {
		return manager, nil
	}
	// 第一次加载：读取 mcp.yaml，连接已启用的服务，并按 tools 白名单发现工具。
	source, err := mcptools.Load(cfg.RegistryFile)
	if err != nil {
		return nil, err
	}
	connections, err := source.Open(lifetime, cfg)
	if err != nil {
		return nil, err
	}
	// Open 自己清理连接阶段的失败；这里负责后续包装或校验失败时回滚。
	defer func() {
		if err != nil {
			connections.Close()
		}
	}()
	if len(connections.Tools) == 0 {
		return nil, fmt.Errorf("没有可用的 MCP 工具；请启用至少一个服务")
	}
	servers := make(map[string]mcptools.Server, len(source.Servers))
	for _, server := range source.Servers {
		servers[server.Name] = server
	}
	seen := make(map[string]bool, len(connections.Tools))
	tools := make([]tool.BaseTool, 0, len(connections.Tools))
	// 只包装发现后的工具：保留 MCP 给出的 schema，再加本应用的执行检查。
	// seen 只用于启动时拒绝重名；运行时的名称分发仍由 EINO 完成。
	for _, base := range connections.Tools {
		info, err := base.Info(lifetime)
		if err != nil {
			return nil, err
		}
		if info == nil || info.Name == "" {
			return nil, fmt.Errorf("工具名称不能为空")
		}
		if seen[info.Name] {
			return nil, fmt.Errorf("工具名称重复: %s", info.Name)
		}
		seen[info.Name] = true
		invokable, ok := base.(tool.InvokableTool)
		if !ok {
			return nil, fmt.Errorf("工具 %s 不支持调用", info.Name)
		}
		// MCP 层把名称改为「服务器名__工具名」，据此找到 path_parameters。
		// 包装对象只保存执行所需字段，不复制命令、认证头等整份服务配置。
		name, _, found := strings.Cut(info.Name, "__")
		server, known := servers[name]
		if !found || !known {
			return nil, fmt.Errorf("工具 %s 没有对应的 MCP 配置", info.Name)
		}
		guarded := &executionTool{
			upstream: invokable, info: info, pathParameters: server.PathParameters,
			timeout: cfg.Timeout, maxBytes: cfg.MaxBytes,
		}
		tools = append(tools, guarded)
	}
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	// 全部成功后才发布；应用退出时自动移除并关闭连接。
	manager := &Manager{key: key, tools: tools, connections: connections, closed: make(chan struct{})}
	sharedManagers.items[key] = manager
	context.AfterFunc(lifetime, manager.Close)
	return manager, nil
}

// Tools 返回可供不同 Agent 筛选的切片副本。
// 复制切片不会复制工具对象和 MCP 连接；关闭后的 Manager 不再交出工具。
func (m *Manager) Tools() ([]tool.BaseTool, error) {
	select {
	case <-m.closed:
		return nil, fmt.Errorf("工具注册表已关闭")
	default:
		return append([]tool.BaseTool(nil), m.tools...), nil
	}
}

// Close 关闭整组共享连接。应用生命周期结束时会自动调用；
// 某个 Agent 或某轮请求结束不能调用它，否则会影响其他使用者。
func (m *Manager) Close() {
	// 显式关闭和 context 取消可能同时发生，Once 保证只关闭一次。
	m.once.Do(func() {
		sharedManagers.Lock()
		defer sharedManagers.Unlock()
		if sharedManagers.items[m.key] == m {
			delete(sharedManagers.items, m.key)
		}
		m.connections.Close()
		close(m.closed)
	})
}
