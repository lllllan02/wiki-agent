// Package config 集中定义程序配置，避免入口、模型和工具各自维护一套默认值。
package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 按职责分组；分组本身递归初始化，具体默认值只写在叶子字段上。
// mapstructure 对应 YAML 键，default 用于反射初始化。
type Config struct {
	Model  Model  `mapstructure:"model"`
	Agent  Agent  `mapstructure:"agent"`
	Server Server `mapstructure:"server"`
	MCP    MCP    `mapstructure:"mcp"`
}

// MCP 只配置接入行为；服务器清单单独保存，避免把外部工具定义写进 Go 代码。
type MCP struct {
	RegistryFile string        `mapstructure:"registry_file" default:""`  // 必填；相对路径按 config.yaml 所在目录解析。
	Timeout      time.Duration `mapstructure:"timeout" default:"30s"`     // 每次握手、发现或调用的超时。
	MaxBytes     int           `mapstructure:"max_bytes" default:"32768"` // 进入模型的单次 MCP 输出上限。
}

// Model 只描述模型连接。密钥和模型名默认留空，不能替用户猜测服务或凭据。
type Model struct {
	APIKey  string        `mapstructure:"api_key" default:""`    // 保存在被 Git 忽略的本地 config.yaml 中，不输出到日志。
	Name    string        `mapstructure:"name" default:""`       // 服务商支持工具调用的模型名称。
	BaseURL string        `mapstructure:"base_url" default:""`   // 空值沿用 EINO 适配器的默认地址。
	Timeout time.Duration `mapstructure:"timeout" default:"60s"` // 有限等待，防止模型请求一直占用服务。
}

// Agent 的步数计数单位是模型请求次数，而不是工具调用数量。
type Agent struct {
	MaxSteps int `mapstructure:"max_steps" default:"8"`
}

// Server 默认只监听本机，避免教学项目未经配置就暴露到局域网。
type Server struct {
	Address string `mapstructure:"address" default:"127.0.0.1:8080"`
}

// Load 先应用标签默认值，再读取指定 YAML；所有运行配置统一来自文件。
// 不绑定环境变量或命令行覆盖，避免用户修改 YAML 后被隐藏的配置来源覆盖。
// 此处只负责加载；Validate 单独检查业务约束。
func Load(path string) (Config, error) {
	cfg, err := NewDefaults[Config]()
	if err != nil {
		return Config{}, err
	}
	// 使用局部 Viper 实例，避免全局状态污染测试或未来的多个入口。
	v := viper.New()
	v.SetConfigType("yaml")
	err = walkFields(reflect.ValueOf(&cfg).Elem(), "", func(key string, field reflect.StructField, value reflect.Value) error {
		v.SetDefault(key, value.Interface())
		return nil
	})
	if err != nil {
		return Config{}, err
	}
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("读取 YAML 配置失败: %w", err)
	}
	// 严格解码能发现 max_step 等拼写错误；不要让配置看似生效但实际被忽略。
	if err := v.UnmarshalExact(&cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置失败: %w", err)
	}
	cfg.Model.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.Model.BaseURL), "/")
	if cfg.MCP.RegistryFile != "" && !filepath.IsAbs(cfg.MCP.RegistryFile) {
		cfg.MCP.RegistryFile = filepath.Join(filepath.Dir(path), cfg.MCP.RegistryFile)
	}
	return cfg, nil
}

// Validate 在调用模型之前报出配置问题，避免把本地配置错误变成远端请求错误。
// 默认对象可以被创建和检查，但空密钥等默认值并不意味着可以直接发起模型请求。
func (c Config) Validate() error {
	if c.Model.BaseURL != "" {
		u, err := url.Parse(c.Model.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("model.base_url 必须是完整的 HTTP(S) API 地址")
		}
	}
	switch {
	case strings.TrimSpace(c.Model.APIKey) == "" || strings.TrimSpace(c.Model.Name) == "":
		return fmt.Errorf("请配置 model.api_key 和 model.name")
	case c.Model.Timeout <= 0:
		return fmt.Errorf("model.timeout 必须大于 0")
	case strings.TrimSpace(c.MCP.RegistryFile) == "":
		return fmt.Errorf("请配置 mcp.registry_file；当前工具由 MCP 注册表提供")
	case c.Agent.MaxSteps <= 0:
		return fmt.Errorf("agent.max_steps 必须大于 0")
	case strings.TrimSpace(c.Server.Address) == "":
		return fmt.Errorf("server.address 不能为空")
	case c.MCP.Timeout <= 0 || c.MCP.MaxBytes <= 0 || c.MCP.MaxBytes > 1024*1024:
		return fmt.Errorf("mcp.timeout 必须大于 0，mcp.max_bytes 必须在 1 到 1048576 之间")
	}
	return nil
}
