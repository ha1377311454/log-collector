package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"log-collector/internal/app"
	"log-collector/internal/config"
	"log-collector/internal/logging"
)

func main() {
	// 命令行只负责选择配置文件；全部采集行为都由 YAML 控制。
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load configuration: %v\n", err)
		os.Exit(1)
	}
	logger, err := logging.New(cfg.Log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// SIGINT/SIGTERM 会触发 Tailer 保存位点并让 Exporter 排空队列。
	defer stop()

	if err := app.Run(ctx, cfg, logger); err != nil {
		logger.Error("collector stopped", "error", err)
		os.Exit(1)
	}
}
