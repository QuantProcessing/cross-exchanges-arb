package trading

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/runlog"
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
	events *runlog.EventSink

	makerName string
	takerName string

	// Baseline balances snapshotted at startup.
	startMakerBal decimal.Decimal
	startTakerBal decimal.Decimal

	// Latest known balances.
	currentMakerBal decimal.Decimal
	currentTakerBal decimal.Decimal

	lastRefresh      time.Time
	rounds           int
	makerFetchFailed bool
	takerFetchFailed bool
	consecutiveFails int
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

func (p *PnLTracker) SetEventSink(events *runlog.EventSink) {
	if p == nil {
		return
	}
	p.events = events
}

// refreshBalances fetches current balances from both exchanges.
func (p *PnLTracker) refreshBalances(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	makerOk := false
	takerOk := false

	if bal, err := p.maker.FetchBalance(fetchCtx); err == nil {
		p.currentMakerBal = bal
		p.makerFetchFailed = false
		makerOk = true
	} else {
		p.logger.Warnf("balance fetch failed %s: %v", p.makerName, err)
		p.makerFetchFailed = true
	}

	if bal, err := p.taker.FetchBalance(fetchCtx); err == nil {
		p.currentTakerBal = bal
		p.takerFetchFailed = false
		takerOk = true
	} else {
		p.logger.Warnf("balance fetch failed %s: %v", p.takerName, err)
		p.takerFetchFailed = true
	}

	if !makerOk || !takerOk {
		p.consecutiveFails++
		if p.consecutiveFails >= 3 {
			p.logger.Warnf("⚠️ balance fetch failed %d times - PnL may be stale", p.consecutiveFails)
		}
	} else {
		p.consecutiveFails = 0
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

	p.logger.Infof("EVT pnl_realized round=%d %s=%s(%+s) %s=%s(%+s) total=%s",
		p.rounds, p.makerName, p.currentMakerBal, makerPnL, p.takerName, p.currentTakerBal, takerPnL, totalPnL)
	p.recordEvent(runlog.Event{
		Category:      "pnl",
		Type:          "realized",
		Round:         p.rounds,
		MakerExchange: p.makerName,
		TakerExchange: p.takerName,
		MakerBalance:  p.currentMakerBal.String(),
		TakerBalance:  p.currentTakerBal.String(),
		MakerPnL:      makerPnL.String(),
		TakerPnL:      takerPnL.String(),
		TotalPnL:      totalPnL.String(),
	})

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

	p.logger.Infof("EVT pnl_refresh %s=%s %s=%s total=%s rounds=%d",
		p.makerName, p.currentMakerBal, p.takerName, p.currentTakerBal, totalPnL, p.rounds)
	p.recordEvent(runlog.Event{
		Category:      "pnl",
		Type:          "refresh",
		MakerExchange: p.makerName,
		TakerExchange: p.takerName,
		MakerBalance:  p.currentMakerBal.String(),
		TakerBalance:  p.currentTakerBal.String(),
		MakerPnL:      makerPnL.String(),
		TakerPnL:      takerPnL.String(),
		TotalPnL:      totalPnL.String(),
		Rounds:        p.rounds,
	})
}

// StartupSummary returns a formatted summary for the startup Telegram notification.
func (p *PnLTracker) StartupSummary() string {
	return fmt.Sprintf("%s: %s\n%s: %s\nTotal: %s",
		p.makerName, p.startMakerBal,
		p.takerName, p.startTakerBal,
		p.startMakerBal.Add(p.startTakerBal))
}

// RoundCount returns the number of completed rounds recorded by the tracker.
func (p *PnLTracker) RoundCount() int {
	if p == nil {
		return 0
	}
	return p.rounds
}

func (p *PnLTracker) recordEvent(event runlog.Event) {
	if p == nil || p.events == nil {
		return
	}
	if err := p.events.Record(event); err != nil && p.logger != nil {
		p.logger.Warnw("event sink write failed", "category", event.Category, "type", event.Type, "err", err)
	}
}
