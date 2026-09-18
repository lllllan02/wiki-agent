// wiki-agent 是程序启动入口，只负责加载配置并启动应用。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/lllllan02/wiki-agent/internal/app"
	"github.com/lllllan02/wiki-agent/internal/config"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func run() error {
	created, err := config.EnsureFile("config.yaml")
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintln(os.Stdout, "已创建 config.yaml，请填写模型配置后重新启动。")
		return nil
	}
	cfg, err := config.Load("config.yaml")
	if err != nil {
		return err
	}
	return app.Run(cfg)
}
