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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var newExchangePair = appx.NewPair
var nowFunc = time.Now
var heartbeatInterval = 5 * time.Second

// Run starts the arbitrage application and blocks until the context is done or the engine exits.
func Run(ctx context.Context, cfg *appconfig.Config, logger *zap.SugaredLogger) (runErr error) {
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

	var pnl *trading.PnLTracker
	defer func() {
		result := "completed"
		detail := ""
		switch {
		case runErr != nil && ctx.Err() == nil:
			result = "error"
			detail = runErr.Error()
		case ctx.Err() != nil:
			result = "canceled"
		}

		rounds := 0
		if pnl != nil {
			rounds = pnl.RoundCount()
		}

		recordSessionEvent(logger, session.Events, runlog.Event{
			At:            nowFunc(),
			Category:      "session",
			Type:          "stopped",
			MakerExchange: cfg.MakerExchange,
			TakerExchange: cfg.TakerExchange,
			Symbol:        cfg.Symbol,
			Result:        result,
			Detail:        detail,
			Rounds:        rounds,
		})
	}()

	logger = teeRunLogger(logger, session.RunLogFile)
	logger.Infow("run artifacts ready",
		"dir", session.Dir,
		"run_log", session.RunLogPath,
		"raw", session.RawPath,
		"events", session.EventsPath,
	)
	recordSessionEvent(logger, session.Events, runlog.Event{
		At:            nowFunc(),
		Category:      "session",
		Type:          "started",
		MakerExchange: cfg.MakerExchange,
		TakerExchange: cfg.TakerExchange,
		Symbol:        cfg.Symbol,
	})

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
		}
	})

	trader := trading.NewTrader(maker, taker, makerAccount, takerAccount, engine, cfg, logger)
	pnl = trading.NewPnLTracker(ctx, maker, taker, cfg.MakerExchange, cfg.TakerExchange, logger)
	pnl.SetEventSink(session.Events)
	trader.SetPnLTracker(pnl)
	trader.SetEventSink(session.Events)
	engine.SetSignalCallback(trader.HandleSignal)

	if err := trader.Start(ctx); err != nil {
		return fmt.Errorf("start trader: %w", err)
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	logger.Infow("starting spread monitoring")
	go runHeartbeatLoop(heartbeatCtx, logger, trader, engine, session.Events)

	go telegram.Notify(fmt.Sprintf("🚀 Cross-Arb Started\n%s ↔ %s | %s\nQty: %s | MaxRounds: %d\nZ-Open: %.1f | Z-Close: %.1f | Z-Stop: %.1f\n──────────\n%s",
		cfg.MakerExchange, cfg.TakerExchange, cfg.Symbol,
		cfg.Quantity, cfg.MaxRounds,
		cfg.ZOpen, cfg.ZClose, cfg.ZStop,
		pnl.StartupSummary()))

	runErr = marketService.Start(ctx)
	stopHeartbeat()
	trader.GracefulShutdown(15 * time.Second)

	go telegram.Notify(fmt.Sprintf("🛑 Cross-Arb Stopped\nRounds completed: %d", pnl.RoundCount()))
	logger.Infow("shutdown complete")

	if runErr != nil && ctx.Err() == nil {
		logger.Errorw("spread engine error", "err", runErr)
	}
	return nil
}

type heartbeatSnapshotter interface {
	Snapshot() *spread.Snapshot
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

func runHeartbeatLoop(ctx context.Context, logger *zap.SugaredLogger, trader *trading.Trader, snapshots heartbeatSnapshotter, events *runlog.EventSink) {
	if logger == nil || trader == nil || snapshots == nil {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			status := trader.OperatorStatusSnapshot(now)
			snapshot := snapshots.Snapshot()
			logHeartbeat(logger, status, snapshot, now)
			recordHealthEvent(logger, events, status, snapshot, now)
		}
	}
}

func logHeartbeat(logger *zap.SugaredLogger, status trading.OperatorStatusSnapshot, snapshot *spread.Snapshot, now time.Time) {
	if logger == nil {
		return
	}
	if snapshot == nil {
		logger.Infof(
			"STAT safe=%s profit=%s blocked=%s state=%s rounds=%d market=warming",
			status.Safe,
			status.Profit,
			status.Blocked,
			status.State,
			status.Rounds,
		)
		return
	}

	logger.Infof(
		"STAT safe=%s profit=%s blocked=%s state=%s rounds=%d maker_lag=%dms taker_lag=%dms",
		status.Safe,
		status.Profit,
		status.Blocked,
		status.State,
		status.Rounds,
		quoteLagMS(now, snapshot.MakerTS),
		quoteLagMS(now, snapshot.TakerTS),
	)
}

func quoteLagMS(local, exchange time.Time) int64 {
	if local.IsZero() || exchange.IsZero() {
		return 0
	}
	return local.Sub(exchange).Milliseconds()
}

func recordSessionEvent(logger *zap.SugaredLogger, events *runlog.EventSink, event runlog.Event) {
	if events == nil {
		return
	}
	if err := events.Record(event); err != nil && logger != nil {
		logger.Warnw("event sink write failed", "category", event.Category, "type", event.Type, "err", err)
	}
}

func recordHealthEvent(logger *zap.SugaredLogger, events *runlog.EventSink, status trading.OperatorStatusSnapshot, snapshot *spread.Snapshot, now time.Time) {
	if events == nil {
		return
	}

	event := runlog.Event{
		At:       now,
		Category: "health",
		Type:     "heartbeat",
		Safe:     string(status.Safe),
		Profit:   status.Profit,
		Blocked:  status.Blocked,
		State:    string(status.State),
		Rounds:   status.Rounds,
	}
	if snapshot == nil {
		event.Market = "warming"
	} else {
		event.MakerLagMS = quoteLagMS(now, snapshot.MakerTS)
		event.TakerLagMS = quoteLagMS(now, snapshot.TakerTS)
	}

	if err := events.Record(event); err != nil && logger != nil {
		logger.Warnw("event sink write failed", "category", event.Category, "type", event.Type, "err", err)
	}
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
