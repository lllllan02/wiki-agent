// Package app 负责启动和关闭本地 Web 服务。
//
// main.go 只适合表达“启动程序”这个动作；这里也只保留必须由应用层关闭的
// HTTP server。模型、WikiAgent 和工具对象由真正使用它们的内部包创建。
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/lllllan02/wiki-agent/internal/agent"
	"github.com/lllllan02/wiki-agent/internal/config"
	"github.com/lllllan02/wiki-agent/internal/web"
)

// Run 根据配置启动本地 Web Agent，并在收到系统停止信号时优雅退出。
func Run(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := newServer(ctx, cfg)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		// 这里不把 Shutdown 错误返回给 main：收到停止信号时，用户更关心服务能退出。
		_ = server.Shutdown(context.Background())
	}()
	fmt.Fprintf(os.Stdout, "Wiki Agent 已启动：http://%s\n按 Ctrl+C 停止服务。\n", cfg.Server.Address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newServer(ctx context.Context, cfg config.Config) (*http.Server, error) {
	wikiAgent, err := agent.NewWikiAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &http.Server{Addr: cfg.Server.Address, Handler: web.New(wikiAgent).Handler()}, nil
}
