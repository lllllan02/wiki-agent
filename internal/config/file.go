package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// EnsureFile 在首次启动时生成可编辑的 YAML，不要求用户先执行复制命令。
// 内容来自反射默认对象，避免模板与字段标签维护两套默认值；已有文件绝不覆盖。
func EnsureFile(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	cfg, err := NewDefaults[Config]()
	if err != nil {
		return false, err
	}
	v := viper.New()
	if err := walkFields(reflect.ValueOf(&cfg).Elem(), "", func(key string, _ reflect.StructField, value reflect.Value) error {
		v.SetDefault(key, value.Interface())
		return nil
	}); err != nil {
		return false, err
	}
	body, err := yaml.Marshal(v.AllSettings())
	if err != nil {
		return false, err
	}
	// O_EXCL 防止检查后另一进程创建文件时被覆盖；0600 限制本地凭据文件的访问权限。
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("创建配置文件失败: %w", err)
	}
	_, writeErr := file.Write(append([]byte("# Wiki Agent 本地配置。填写 model.api_key、model.name 和 model.base_url。\n# 修改后重新启动服务。此文件可能含密钥，不要提交或分享。\n"), body...))
	closeErr := file.Close()
	if writeErr != nil {
		return false, writeErr
	}
	return true, closeErr
}
