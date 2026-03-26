package main

import (
	"context"
	"fmt"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// PnLTracker tracks balances and PnL across two exchanges.
type PnLTracker struct {
	maker  exchanges.Exchange
	taker  exchanges.Exchange
	logger *zap.SugaredLogger

	makerName string
	takerName string

	// Baseline balances snapshotted at startup.
	startMakerBal decimal.Decimal
	startTakerBal decimal.Decimal

	// Latest known balances.
	currentMakerBal decimal.Decimal
	currentTakerBal decimal.Decimal

	lastRefresh time.Time
	rounds      int
}

// NewPnLTracker creates a tracker and snapshots initial balances.
func NewPnLTracker(ctx context.Context, maker, taker exchanges.Exchange, makerName, takerName string, logger *zap.SugaredLogger) *PnLTracker {
	p := &PnLTracker{
		maker:     maker,
		taker:     taker,
		logger:    logger,
		makerName: makerName,
		takerName: takerName,
	}
	p.refreshBalances(ctx)
	p.startMakerBal = p.currentMakerBal
	p.startTakerBal = p.currentTakerBal
	return p
}

// refreshBalances fetches current balances from both exchanges.
func (p *PnLTracker) refreshBalances(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if bal, err := p.maker.FetchBalance(fetchCtx); err == nil {
		p.currentMakerBal = bal
	} else {
		p.logger.Warnw("failed to fetch maker balance", "exchange", p.makerName, "err", err)
	}

	if bal, err := p.taker.FetchBalance(fetchCtx); err == nil {
		p.currentTakerBal = bal
	} else {
		p.logger.Warnw("failed to fetch taker balance", "exchange", p.takerName, "err", err)
	}

	p.lastRefresh = time.Now()
}

// OnRoundComplete refreshes balances and logs PnL after a successful close.
func (p *PnLTracker) OnRoundComplete(ctx context.Context) {
	p.rounds++
	p.refreshBalances(ctx)

	makerPnL := p.currentMakerBal.Sub(p.startMakerBal)
	takerPnL := p.currentTakerBal.Sub(p.startTakerBal)
	totalPnL := makerPnL.Add(takerPnL)

	p.logger.Infow("💰 round complete — PnL update",
		"round", p.rounds,
		p.makerName, fmt.Sprintf("%s (PnL: %s)", p.currentMakerBal, makerPnL),
		p.takerName, fmt.Sprintf("%s (PnL: %s)", p.currentTakerBal, takerPnL),
		"totalPnL", totalPnL,
	)

	go telegram.Notify(fmt.Sprintf("💰 Round %d Complete\n%s: %s (PnL: %s)\n%s: %s (PnL: %s)\nTotal PnL: %s",
		p.rounds,
		p.makerName, p.currentMakerBal, makerPnL,
		p.takerName, p.currentTakerBal, takerPnL,
		totalPnL))
}

// PeriodicRefresh refreshes balances if enough time has passed (5 minutes).
func (p *PnLTracker) PeriodicRefresh(ctx context.Context) {
	if time.Since(p.lastRefresh) < 5*time.Minute {
		return
	}
	p.refreshBalances(ctx)

	makerPnL := p.currentMakerBal.Sub(p.startMakerBal)
	takerPnL := p.currentTakerBal.Sub(p.startTakerBal)
	totalPnL := makerPnL.Add(takerPnL)

	p.logger.Infow("📊 balance check",
		p.makerName, p.currentMakerBal,
		p.takerName, p.currentTakerBal,
		"totalPnL", totalPnL,
		"rounds", p.rounds,
	)
}

// StartupSummary returns a formatted summary for the startup Telegram notification.
func (p *PnLTracker) StartupSummary() string {
	return fmt.Sprintf("%s: %s\n%s: %s\nTotal: %s",
		p.makerName, p.startMakerBal,
		p.takerName, p.startTakerBal,
		p.startMakerBal.Add(p.startTakerBal))
}
