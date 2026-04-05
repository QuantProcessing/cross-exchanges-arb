package app

import (
	"context"
	"fmt"
	"os"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	appx "github.com/QuantProcessing/cross-exchanges-arb/internal/exchange"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/marketdata"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/runlog"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/trading"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/exchanges/account"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var newExchangePair = appx.NewPair
var nowFunc = time.Now

// Run starts the arbitrage application and blocks until the context is done or the engine exits.
func Run(ctx context.Context, cfg *appconfig.Config, logger *zap.SugaredLogger) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}

	session, err := runlog.NewSession("logs", runlog.SessionOptions{
		MakerExchange: cfg.MakerExchange,
		TakerExchange: cfg.TakerExchange,
		Symbol:        cfg.Symbol,
		StartedAt:     nowFunc(),
	})
	if err != nil {
		return fmt.Errorf("create run log session: %w", err)
	}
	defer session.Close()

	logger = teeRunLogger(logger, session.RunLogFile)
	logger.Infow("run artifacts ready",
		"dir", session.Dir,
		"run_log", session.RunLogPath,
		"raw", session.RawPath,
	)

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

	makerAccount := account.NewTradingAccount(maker, logger)
	if err := makerAccount.Start(ctx); err != nil {
		return fmt.Errorf("start maker account: %w", err)
	}
	defer makerAccount.Close()

	takerAccount := account.NewTradingAccount(taker, logger)
	if err := takerAccount.Start(ctx); err != nil {
		return fmt.Errorf("start taker account: %w", err)
	}
	defer takerAccount.Close()

	engine := spread.New(cfg, logger)
	engine.SetFees(makerFee, takerFee)
	defer engine.Close()

	marketService := marketdata.NewService(maker, taker, cfg.Symbol, cfg.MakerExchange, cfg.TakerExchange)
	marketService.Subscribe(engine.OnMarketFrame)

	recorder := marketdata.NewRecorder(cfg.MakerExchange, cfg.TakerExchange, cfg.Symbol, session.RawFile)
	marketService.Subscribe(func(frame marketdata.MarketFrame) {
		if err := recorder.Record(frame); err != nil {
			logger.Warnw("raw recorder write failed", "err", err)
			return
		}
		if snapshot := engine.Snapshot(); snapshot != nil {
			logMarketSnapshot(logger, frame, snapshot)
		}
	})

	trader := trading.NewTrader(maker, taker, makerAccount, takerAccount, engine, cfg, logger)
	pnl := trading.NewPnLTracker(ctx, maker, taker, cfg.MakerExchange, cfg.TakerExchange, logger)
	trader.SetPnLTracker(pnl)
	engine.SetSignalCallback(trader.HandleSignal)

	if err := trader.Start(ctx); err != nil {
		return fmt.Errorf("start trader: %w", err)
	}

	logger.Infow("starting spread monitoring")

	go telegram.Notify(fmt.Sprintf("🚀 Cross-Arb Started\n%s ↔ %s | %s\nQty: %s | MaxRounds: %d\nZ-Open: %.1f | Z-Close: %.1f | Z-Stop: %.1f\n──────────\n%s",
		cfg.MakerExchange, cfg.TakerExchange, cfg.Symbol,
		cfg.Quantity, cfg.MaxRounds,
		cfg.ZOpen, cfg.ZClose, cfg.ZStop,
		pnl.StartupSummary()))

	runErr := marketService.Start(ctx)
	trader.GracefulShutdown(15 * time.Second)

	go telegram.Notify(fmt.Sprintf("🛑 Cross-Arb Stopped\nRounds completed: %d", pnl.RoundCount()))
	logger.Infow("shutdown complete")

	if runErr != nil && ctx.Err() == nil {
		logger.Errorw("spread engine error", "err", runErr)
	}
	return nil
}

func teeRunLogger(base *zap.SugaredLogger, runLogFile *os.File) *zap.SugaredLogger {
	if base == nil {
		base = zap.NewNop().Sugar()
	}
	if runLogFile == nil {
		return base
	}

	encoderCfg := zap.NewDevelopmentEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	fileCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderCfg),
		zapcore.AddSync(runLogFile),
		zap.DebugLevel,
	)
	return zap.New(zapcore.NewTee(base.Desugar().Core(), fileCore)).Sugar()
}

type marketStats struct {
	SpreadABBps     float64
	SpreadBABps     float64
	MeanAB          float64
	MeanBA          float64
	StdAB           float64
	StdBA           float64
	ZAB             float64
	ZBA             float64
	MakerQuoteLagMS int64
	TakerQuoteLagMS int64
}

func metricsStatsFromSnapshot(snapshot *spread.Snapshot, frame marketdata.MarketFrame) marketStats {
	if snapshot == nil {
		return marketStats{
			MakerQuoteLagMS: quoteLagMS(frame.LocalTime, frame.MakerExchangeTS),
			TakerQuoteLagMS: quoteLagMS(frame.LocalTime, frame.TakerExchangeTS),
		}
	}

	return marketStats{
		SpreadABBps:     roundBps(snapshot.SpreadAB),
		SpreadBABps:     roundBps(snapshot.SpreadBA),
		MeanAB:          roundBps(snapshot.MeanAB),
		MeanBA:          roundBps(snapshot.MeanBA),
		StdAB:           roundBps(snapshot.StdDevAB),
		StdBA:           roundBps(snapshot.StdDevBA),
		ZAB:             zScore(snapshot.SpreadAB, snapshot.MeanAB, snapshot.StdDevAB),
		ZBA:             zScore(snapshot.SpreadBA, snapshot.MeanBA, snapshot.StdDevBA),
		MakerQuoteLagMS: quoteLagMS(frame.LocalTime, frame.MakerExchangeTS),
		TakerQuoteLagMS: quoteLagMS(frame.LocalTime, frame.TakerExchangeTS),
	}
}

func quoteLagMS(local, exchange time.Time) int64 {
	if local.IsZero() || exchange.IsZero() {
		return 0
	}
	return local.Sub(exchange).Milliseconds()
}

func zScore(value, mean, stddev float64) float64 {
	if stddev < 1e-9 {
		return 0
	}
	return (value - mean) / stddev
}

func roundBps(value float64) float64 {
	rounded, _ := decimal.NewFromFloat(value).Round(1).Float64()
	return rounded
}

func logMarketSnapshot(logger *zap.SugaredLogger, frame marketdata.MarketFrame, snapshot *spread.Snapshot) {
	if logger == nil || snapshot == nil {
		return
	}
	stats := metricsStatsFromSnapshot(snapshot, frame)

	logger.Infof(
		"MKT updated=%s mbid=%s mbid_qty=%s mask=%s mask_qty=%s tbid=%s tbid_qty=%s task=%s task_qty=%s ab=%.1f ba=%.1f mean_ab=%.1f std_ab=%.1f z_ab=%.2f mean_ba=%.1f std_ba=%.1f z_ba=%.2f maker_lag=%dms taker_lag=%dms valid_ab=%t valid_ba=%t reason_ab=%q reason_ba=%q",
		frame.UpdatedSide,
		snapshot.MakerBid, snapshot.MakerBidQty,
		snapshot.MakerAsk, snapshot.MakerAskQty,
		snapshot.TakerBid, snapshot.TakerBidQty,
		snapshot.TakerAsk, snapshot.TakerAskQty,
		stats.SpreadABBps, stats.SpreadBABps,
		stats.MeanAB, stats.StdAB, stats.ZAB,
		stats.MeanBA, stats.StdBA, stats.ZBA,
		stats.MakerQuoteLagMS, stats.TakerQuoteLagMS,
		snapshot.ValidAB, snapshot.ValidBA,
		snapshot.ReasonAB, snapshot.ReasonBA,
	)
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
