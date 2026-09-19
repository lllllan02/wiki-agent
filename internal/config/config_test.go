package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 默认值是代码中的单一事实来源；嵌套配置和独立分组必须走同一初始化规则。
func TestNewDefaults(t *testing.T) {
	cfg, err := NewDefaults[Config]()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Timeout != time.Minute || cfg.Model.APIKey != "" || cfg.Agent.MaxSteps != 8 {
		t.Fatalf("默认值不符合预期: %+v", cfg)
	}
	model, err := NewDefaults[Model]()
	if err != nil || model != cfg.Model {
		t.Fatalf("独立分组未初始化: %+v %v", model, err)
	}
}

func yamlFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// YAML 是唯一覆盖来源；宿主环境不应改变文件中的模型或运行设置。
func TestLoadYAML(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "ignored-model")
	t.Setenv("WIKI_AGENT_AGENT_MAX_STEPS", "99")
	path := yamlFile(t, "model:\n  api_key: test-key\n  name: yaml-model\n  timeout: 12s\nagent:\n  max_steps: 12\nmcp:\n  registry_file: mcp.yaml\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Name != "yaml-model" || cfg.Model.Timeout != 12*time.Second || cfg.Agent.MaxSteps != 12 {
		t.Fatal("YAML 或默认值没有正确加载")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(yamlFile(t, "agent:\n  max_steps: 0\n"))
	if err != nil || cfg.Agent.MaxSteps != 0 || cfg.Validate() == nil {
		t.Fatal("显式零值不能被默认值覆盖")
	}
}

// 文件缺失、键名拼错和无法解析的值必须明确报错，方便在网页启动前修复配置。
func TestLoadFilesAndErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("缺失文件应该失败")
	}
	for name, body := range map[string]string{
		"拼写错误":    "agent:\n  max_step: 2\n",
		"已删除字段":   "wiki:\n  root: wiki\n",
		"类型错误":    "agent:\n  max_steps: abc\n",
		"时间错误":    "model:\n  timeout: tomorrow\n",
		"YAML 损坏": "model: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(yamlFile(t, body)); err == nil {
				t.Fatal("应该拒绝错误配置")
			}
		})
	}
}

func TestMCPRegistryPath(t *testing.T) {
	path := yamlFile(t, "mcp:\n  registry_file: mcp.local.yaml\n  timeout: 9s\n  max_bytes: 2048\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCP.RegistryFile != filepath.Join(filepath.Dir(path), "mcp.local.yaml") || cfg.MCP.Timeout != 9*time.Second || cfg.MCP.MaxBytes != 2048 {
		t.Fatalf("MCP 配置未正确解析: %+v", cfg.MCP)
	}
}

// 自动生成的 YAML 必须能够重新加载，且重复启动不能覆盖用户填写的内容。
func TestEnsureFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	created, err := EnsureFile(path)
	if err != nil || !created {
		t.Fatalf("创建失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Model.Timeout != time.Minute {
		t.Fatalf("生成文件不能还原默认值: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("配置文件权限应为 0600")
	}
	content := []byte("model:\n  name: user-model\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	created, err = EnsureFile(path)
	if err != nil || created {
		t.Fatalf("不应重写已有文件: %v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(content) {
		t.Fatal("已有配置被覆盖")
	}
}

func TestValidation(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"缺密钥":     func(c *Config) { c.Model.APIKey = "" },
		"缺模型":     func(c *Config) { c.Model.Name = " " },
		"超时无效":    func(c *Config) { c.Model.Timeout = 0 },
		"未配置 MCP": func(c *Config) { c.MCP.RegistryFile = "" },
		"步数无效":    func(c *Config) { c.Agent.MaxSteps = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := NewDefaults[Config]()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Model.APIKey, cfg.Model.Name = "test-key", "test-model"
			cfg.MCP.RegistryFile = "mcp.yaml"
			change(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("应该拒绝无效配置")
			}
		})
	}
}

// 反射必须及早报告标签错误、溢出和不支持的类型，不能在启动时 panic 或默默给零值。
func TestInvalidDefaults(t *testing.T) {
	type missing struct {
		Value int `mapstructure:"value"`
	}
	type overflow struct {
		Value int8 `mapstructure:"value" default:"128"`
	}
	type invalid struct {
		Value bool `mapstructure:"value" default:"maybe"`
	}
	type unsupported struct {
		Value []string `mapstructure:"value" default:""`
	}
	type duration struct {
		Value time.Duration `mapstructure:"value" default:"abc"`
	}
	checks := []func() error{
		func() error { _, err := NewDefaults[missing](); return err },
		func() error { _, err := NewDefaults[overflow](); return err },
		func() error { _, err := NewDefaults[invalid](); return err },
		func() error { _, err := NewDefaults[unsupported](); return err },
		func() error { _, err := NewDefaults[duration](); return err },
		func() error { _, err := NewDefaults[*Config](); return err },
	}
	for i, check := range checks {
		if check() == nil {
			t.Fatalf("第 %d 个错误默认值未被拒绝", i)
		}
	}
}
