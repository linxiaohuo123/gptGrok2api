// [INPUT]: internal/config 的配置装载，internal/httpapi 的 Server 装配
// [OUTPUT]: 进程入口：main
// [POS]: 进程入口。装载配置 → 组装 Server → 起监听 → 等待信号优雅退出。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/httpapi"
)

func main() {
	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	logFile, logErr := os.OpenFile(filepath.Join(cfg.RootDir, "logs", "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if logErr == nil {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	} else {
		log.Printf("open runtime log: %v", logErr)
	}

	api := httpapi.New(cfg)
	server := &http.Server{
		Addr:                         cfg.ListenAddr,
		Handler:                      api.Handler(),
		ReadHeaderTimeout:            10 * time.Second,
		ReadTimeout:                  cfg.RequestTimeout,
		WriteTimeout:                 0,
		IdleTimeout:                  120 * time.Second,
		MaxHeaderBytes:               1 << 20,
		DisableGeneralOptionsHandler: false,
		// Streaming endpoints deliberately keep WriteTimeout disabled.
	}

	go func() {
		log.Printf("GPT2API listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	api.Shutdown()
}
