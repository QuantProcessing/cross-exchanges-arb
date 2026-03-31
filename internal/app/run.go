package app

import (
	"context"
	"fmt"
	"os"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	appx "github.com/QuantProcessing/cross-exchanges-arb/internal/exchange"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/trading"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
	"go.uber.org/zap"
)

var newExchangePair = appx.NewPair

// Run starts the arbitrage application and blocks until the context is done or the engine exits.
func Run(ctx context.Context, cfg *appconfig.Config, logger *zap.SugaredLogger) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}

	initTelegram(logger)
	fmt.Println(cfg.String())

	logger.Infow("creating adapters...")
	maker, taker, err := newExchangePair(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create exchange pair: %w", err)
	}
	defer maker.Close()
	defer taker.Close()

	printExchangeInfo(ctx, maker, cfg.Symbol, cfg.MakerExchange, logger)
	printExchangeInfo(ctx, taker, cfg.Symbol, cfg.TakerExchange, logger)
	fmt.Println("════════════════════════════════════════════════════════")

	makerFee := loadFeeRate(ctx, maker, cfg.Symbol, cfg.MakerExchange, logger)
	takerFee := loadFeeRate(ctx, taker, cfg.Symbol, cfg.TakerExchange, logger)

	engine := spread.New(maker, taker, cfg, logger)
	engine.SetFees(makerFee, takerFee)
	defer engine.Close()

	if cfg.ObserveOnly {
		logger.Infow("OBSERVE-ONLY MODE: collecting spread data to CSV. Press Ctrl+C to stop.")
		if err := engine.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Errorw("spread engine error", "err", err)
		}
		return nil
	}

	trader := trading.NewTrader(maker, taker, engine, cfg, logger)
	pnl := trading.NewPnLTracker(ctx, maker, taker, cfg.MakerExchange, cfg.TakerExchange, logger)
	trader.SetPnLTracker(pnl)
	engine.SetSignalCallback(trader.HandleSignal)

	if err := trader.Start(ctx); err != nil {
		return fmt.Errorf("start trader: %w", err)
	}

	mode := "LIVE"
	if cfg.DryRun {
		mode = "DRY RUN"
	}
	logger.Infow("starting spread monitoring", "mode", mode)

	go telegram.Notify(fmt.Sprintf("🚀 Cross-Arb Started (%s)\n%s ↔ %s | %s\nQty: %s | MaxRounds: %d\nZ-Open: %.1f | Z-Close: %.1f | Z-Stop: %.1f\n──────────\n%s",
		mode, cfg.MakerExchange, cfg.TakerExchange, cfg.Symbol,
		cfg.Quantity, cfg.MaxRounds,
		cfg.ZOpen, cfg.ZClose, cfg.ZStop,
		pnl.StartupSummary()))

	runErr := engine.Start(ctx)
	if !cfg.ObserveOnly {
		trader.GracefulShutdown(15 * time.Second)
	}

	go telegram.Notify(fmt.Sprintf("🛑 Cross-Arb Stopped\nRounds completed: %d", pnl.RoundCount()))
	logger.Infow("shutdown complete")

	if runErr != nil && ctx.Err() == nil {
		logger.Errorw("spread engine error", "err", runErr)
	}
	return nil
}

func initTelegram(logger *zap.SugaredLogger) {
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
}

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

func loadFeeRate(ctx context.Context, adp exchanges.Exchange, symbol, name string, logger *zap.SugaredLogger) spread.FeeInfo {
	fee, err := adp.FetchFeeRate(ctx, symbol)
	if err != nil {
		logger.Warnw("failed to fetch fee rate, using defaults", "exchange", name, "err", err)
		return spread.FeeInfo{MakerRate: 0.0002, TakerRate: 0.0005}
	}

	makerRate, _ := fee.Maker.Float64()
	takerRate, _ := fee.Taker.Float64()

	fmt.Printf("  %-10s Fees: Maker=%.4f%% Taker=%.4f%%\n", name, makerRate*100, takerRate*100)
	return spread.FeeInfo{MakerRate: makerRate, TakerRate: takerRate}
}
