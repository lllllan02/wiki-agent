package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// NewDefaults 创建带标签默认值的配置对象，也适用于 Model 等单独分组。
// Go 不会在 Config{} 或 new(Config) 时执行构造函数，因此业务代码必须使用此入口或 Load。
// 不对已有对象“补零值”：false、0、空字符串可能是用户明确选择，不能据此判断字段未配置。
func NewDefaults[T any]() (T, error) {
	var cfg T
	err := walkFields(reflect.ValueOf(&cfg).Elem(), "", func(key string, field reflect.StructField, value reflect.Value) error {
		raw, ok := field.Tag.Lookup("default")
		// Lookup 能区分缺少标签与 default:""；后者是合法、明确的空字符串默认值。
		if !ok {
			return fmt.Errorf("配置字段 %s 缺少 default 标签", key)
		}
		if err := setDefault(value, raw); err != nil {
			return fmt.Errorf("配置字段 %s 的默认值无效: %w", key, err)
		}
		return nil
	})
	return cfg, err
}

// walkFields 递归遍历值类型的配置分组，拼出 model.timeout 这样的 Viper 键。
// 当前只支持嵌套结构体和标量；未支持的类型明确报错，避免反射悄悄跳过字段。
func walkFields(value reflect.Value, prefix string, visit func(string, reflect.StructField, reflect.Value) error) error {
	if value.Kind() != reflect.Struct {
		return fmt.Errorf("配置对象必须是结构体值")
	}
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		name := field.Tag.Get("mapstructure")
		if field.PkgPath != "" || name == "" || strings.ContainsAny(name, ",.") || name == "-" {
			return fmt.Errorf("配置字段 %s 必须导出并声明简单的 mapstructure 标签", field.Name)
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		child := value.Field(i)
		if child.Kind() == reflect.Struct {
			if err := walkFields(child, key, visit); err != nil {
				return err
			}
		} else if err := visit(key, field, child); err != nil {
			return err
		}
	}
	return nil
}

// setDefault 严格转换标签字符串；错误和整数溢出在启动时暴露。
// time.Duration 底层也是 int64，但标签使用 60s 等可读单位，所以要先单独处理。
func setDefault(value reflect.Value, raw string) error {
	if value.Type() == reflect.TypeOf(time.Duration(0)) {
		duration, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		value.SetInt(int64(duration))
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString(raw)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		value.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(raw, 10, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(raw, 10, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetUint(parsed)
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(raw, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetFloat(parsed)
	default:
		return fmt.Errorf("暂不支持配置类型 %s", value.Type())
	}
	return nil
}
