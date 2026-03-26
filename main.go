package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	exchanges "github.com/QuantProcessing/exchanges"
	_ "github.com/QuantProcessing/exchanges/decibel"
	_ "github.com/QuantProcessing/exchanges/edgex"
	_ "github.com/QuantProcessing/exchanges/lighter"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

func main() {
	// Load .env file (ignore error if not present)
	godotenv.Load()

	// Parse config
	cfg := ParseConfig()

	// Create logger
	zapCfg := zap.NewDevelopmentConfig()
	zapCfg.DisableStacktrace = true
	zapLogger, err := zapCfg.Build()
	if err != nil {
		panic(err)
	}
	defer zapLogger.Sync()
	logger := zapLogger.Sugar()

	// Initialize Telegram notifications (optional)
	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		if err := telegram.Init(telegram.Config{
			BotToken: token,
			ChatID:   os.Getenv("TELEGRAM_CHAT_ID"),
		}); err != nil {
			logger.Warnw("telegram init failed", "err", err)
		} else {
			logger.Infow("telegram notifications enabled")
		}
	}

	// Print startup banner
	fmt.Println(cfg.String())

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Infow("received signal, shutting down...", "signal", sig)
		cancel()
	}()

	// Create adapters directly
	logger.Infow("creating adapters...")
	maker, taker, err := NewExchangePair(ctx, cfg)
	if err != nil {
		logger.Fatalw("failed to create exchange pair", "maker", cfg.MakerExchange, "taker", cfg.TakerExchange, "err", err)
	}
	defer maker.Close()
	defer taker.Close()

	// Print exchange info
	printExchangeInfo(ctx, maker, cfg.Symbol, cfg.MakerExchange, logger)
	printExchangeInfo(ctx, taker, cfg.Symbol, cfg.TakerExchange, logger)
	fmt.Println("════════════════════════════════════════════════════════")

	// Load fee rates
	makerFee := loadFeeRate(ctx, maker, cfg.Symbol, cfg.MakerExchange, logger)
	takerFee := loadFeeRate(ctx, taker, cfg.Symbol, cfg.TakerExchange, logger)

	// Create spread engine
	engine := NewSpreadEngine(maker, taker, cfg, logger)
	engine.SetFees(makerFee, takerFee)
	defer engine.Close()

	if cfg.ObserveOnly {
		logger.Infow("🔍 OBSERVE-ONLY MODE: collecting spread data to CSV. Press Ctrl+C to stop.")
		if err := engine.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Errorw("spread engine error", "err", err)
		}
		return
	}

	// Create trader
	trader := NewTrader(maker, taker, engine, cfg, logger)

	// Initialize PnL tracker (snapshots starting balances).
	pnl := NewPnLTracker(ctx, maker, taker, cfg.MakerExchange, cfg.TakerExchange, logger)
	trader.SetPnLTracker(pnl)

	// Connect signal callback
	engine.SetSignalCallback(trader.HandleSignal)

	// Start trader monitoring
	trader.Start(ctx)

	mode := "LIVE"
	if cfg.DryRun {
		mode = "DRY RUN"
	}
	logger.Infow("🚀 Starting spread monitoring...", "mode", mode)

	// Send startup notification.
	go telegram.Notify(fmt.Sprintf("🚀 Cross-Arb Started (%s)\n%s ↔ %s | %s\nQty: %s | MaxRounds: %d\nZ-Open: %.1f | Z-Close: %.1f | Z-Stop: %.1f\n──────────\n%s",
		mode, cfg.MakerExchange, cfg.TakerExchange, cfg.Symbol,
		cfg.Quantity, cfg.MaxRounds,
		cfg.ZOpen, cfg.ZClose, cfg.ZStop,
		pnl.StartupSummary()))

	// Start spread engine (blocks until context done or error)
	if err := engine.Start(ctx); err != nil && ctx.Err() == nil {
		logger.Errorw("spread engine error", "err", err)
	}

	go telegram.Notify(fmt.Sprintf("🛑 Cross-Arb Stopped\nRounds completed: %d", pnl.rounds))
	logger.Infow("shutdown complete")
}

// printExchangeInfo prints exchange price and symbol details at startup.
func printExchangeInfo(ctx context.Context, adp exchanges.Exchange, symbol, name string, logger *zap.SugaredLogger) {
	ticker, err := adp.FetchTicker(ctx, symbol)
	if err != nil {
		logger.Warnw("failed to fetch ticker", "exchange", name, "err", err)
		return
	}

	details, err := adp.FetchSymbolDetails(ctx, symbol)
	if err != nil {
		logger.Warnw("failed to fetch symbol details", "exchange", name, "err", err)
		fmt.Printf("  %-10s %s Price: %s\n", name, symbol, ticker.LastPrice)
		return
	}

	fmt.Printf("  %-10s %s Price: %s  (PricePrecision=%d, QtyPrecision=%d, MinQty=%s)\n",
		name, symbol, ticker.LastPrice,
		details.PricePrecision, details.QuantityPrecision, details.MinQuantity)
}

// loadFeeRate fetches fee rates for an exchange.
func loadFeeRate(ctx context.Context, adp exchanges.Exchange, symbol, name string, logger *zap.SugaredLogger) FeeInfo {
	fee, err := adp.FetchFeeRate(ctx, symbol)
	if err != nil {
		logger.Warnw("failed to fetch fee rate, using defaults",
			"exchange", name, "err", err)
		return FeeInfo{MakerRate: 0.0002, TakerRate: 0.0005} // conservative defaults
	}

	makerRate, _ := fee.Maker.Float64()
	takerRate, _ := fee.Taker.Float64()

	fmt.Printf("  %-10s Fees: Maker=%.4f%% Taker=%.4f%%\n",
		name, makerRate*100, takerRate*100)

	return FeeInfo{MakerRate: makerRate, TakerRate: takerRate}
}
