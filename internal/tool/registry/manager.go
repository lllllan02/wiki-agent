// Package registry 管理共享 MCP 连接，并将标准工具与中间件注册到 EINO。
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
	// Open 清理连接阶段的失败；这里负责发布前失败时回滚。
	defer func() {
		if err != nil {
			connections.Close()
		}
	}()
	if len(connections.Tools) == 0 {
		return nil, fmt.Errorf("没有可用的 MCP 工具；请启用至少一个服务")
	}
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	// 全部成功后才发布；应用退出时自动移除并关闭连接。
	manager := &Manager{key: key, tools: append([]tool.BaseTool(nil), connections.Tools...), connections: connections, closed: make(chan struct{})}
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
