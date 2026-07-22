package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/app"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	config, err := app.LoadConfig(os.Getenv)
	if err != nil {
		logger.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}
	controlPlane, err := app.New(config, logger)
	if err != nil {
		logger.Error("Initialize control plane", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := controlPlane.Run(ctx); err != nil {
		logger.Error("Control plane stopped", "error", err)
		os.Exit(1)
	}
}
