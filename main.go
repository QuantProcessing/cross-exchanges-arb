package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/app"
	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

func main() {
	_ = godotenv.Load()

	zapCfg := zap.NewDevelopmentConfig()
	zapCfg.DisableStacktrace = true
	zapLogger, err := zapCfg.Build()
	if err != nil {
		panic(err)
	}
	defer zapLogger.Sync()
	logger := zapLogger.Sugar()

	cfg := appconfig.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg, logger); err != nil && ctx.Err() == nil {
		logger.Fatalw("application exited with error", "err", err)
	}
}
