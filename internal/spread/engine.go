package spread

import (
	"sync"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/marketdata"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// Engine monitors BBO from two exchanges and detects arbitrage signals.
type Engine struct {
	symbol string
	config *appconfig.Config
	logger *zap.SugaredLogger

	mu          sync.Mutex
	makerBid    decimal.Decimal
	makerAsk    decimal.Decimal
	takerBid    decimal.Decimal
	takerAsk    decimal.Decimal
	makerBidQty decimal.Decimal
	makerAskQty decimal.Decimal
	takerBidQty decimal.Decimal
	takerAskQty decimal.Decimal
	makerTS     time.Time
	takerTS     time.Time

	// Rolling stats for both directions
	statsAB *Stats // spread: takerBid - makerAsk (Long Maker, Short Taker)
	statsBA *Stats // spread: makerBid - takerAsk (Long Taker, Short Maker)

	// Warmup tracking
	firstSeen    time.Time
	tickCount    int
	warmupLogged int // bitmask: 1=25%, 2=50%, 4=75%, 8=100%, 16=complete

	// Fee rates (loaded at startup)
	makerFee FeeInfo
	takerFee FeeInfo

	// Signal callback
	onSignal       func(signal *Signal)
	onMarketUpdate func()
}

// New creates a new spread engine.
func New(cfg *appconfig.Config, logger *zap.SugaredLogger) *Engine {
	return &Engine{
		symbol:  cfg.Symbol,
		config:  cfg,
		logger:  logger,
		statsAB: NewStats(cfg.WindowSize),
		statsBA: NewStats(cfg.WindowSize),
	}
}

// SetFees configures the fee rates for both exchanges.
func (e *Engine) SetFees(makerFee, takerFee FeeInfo) {
	e.makerFee = makerFee
	e.takerFee = takerFee
}

// Fees returns the fee schedules currently configured on the engine.
func (e *Engine) Fees() (maker FeeInfo, taker FeeInfo) {
	if e == nil {
		return FeeInfo{}, FeeInfo{}
	}
	return e.makerFee, e.takerFee
}

// SetSignalCallback sets the callback for when a signal is detected.
func (e *Engine) SetSignalCallback(cb func(signal *Signal)) {
	e.onSignal = cb
}

// SetMarketUpdateCallback sets the callback for any top-of-book update.
func (e *Engine) SetMarketUpdateCallback(cb func()) {
	e.onMarketUpdate = cb
}

// RoundTripFeeBps calculates the total round-trip fee in BPS.
// A round-trip involves 4 fee events: maker open, taker hedge open, maker close,
// and taker close. The current execution path opens on the maker exchange with a
// maker order, then closes both sides with market orders.
func (e *Engine) RoundTripFeeBps() float64 {
	return (e.makerFee.MakerRate + e.makerFee.TakerRate + e.takerFee.TakerRate + e.takerFee.TakerRate) * 10000
}

// OnMarketFrame applies a bilateral market snapshot emitted by the shared
// marketdata service.
func (e *Engine) OnMarketFrame(frame marketdata.MarketFrame) {
	e.mu.Lock()
	e.makerBid, e.makerBidQty = firstLevel(frame.MakerBook.Bids)
	e.makerAsk, e.makerAskQty = firstLevel(frame.MakerBook.Asks)
	e.takerBid, e.takerBidQty = firstLevel(frame.TakerBook.Bids)
	e.takerAsk, e.takerAskQty = firstLevel(frame.TakerBook.Asks)
	e.makerTS = frame.MakerExchangeTS
	e.takerTS = frame.TakerExchangeTS
	e.mu.Unlock()
	e.onBBOUpdate()
}

// Close cleans up resources.
func (e *Engine) Close() {
}

// onBBOUpdate is called whenever either exchange's BBO updates.
// It is called concurrently from maker and taker WatchOrderBook goroutines.
func (e *Engine) onBBOUpdate() {
	e.mu.Lock()
	makerRaw := TopOfBook{
		BidPrice:  e.makerBid,
		BidQty:    e.makerBidQty,
		AskPrice:  e.makerAsk,
		AskQty:    e.makerAskQty,
		Timestamp: e.makerTS,
	}
	takerRaw := TopOfBook{
		BidPrice:  e.takerBid,
		BidQty:    e.takerBidQty,
		AskPrice:  e.takerAsk,
		AskQty:    e.takerAskQty,
		Timestamp: e.takerTS,
	}
	e.mu.Unlock()

	maker := SanitizeTopOfBook(makerRaw)
	taker := SanitizeTopOfBook(takerRaw)

	// Need all four prices
	if maker.BidPrice.IsZero() || maker.AskPrice.IsZero() || taker.BidPrice.IsZero() || taker.AskPrice.IsZero() {
		return
	}

	// Calculate mid price for normalization
	makerMid := maker.BidPrice.Add(maker.AskPrice).Div(decimal.NewFromInt(2))
	takerMid := taker.BidPrice.Add(taker.AskPrice).Div(decimal.NewFromInt(2))
	midPrice := makerMid.Add(takerMid).Div(decimal.NewFromInt(2))

	if midPrice.IsZero() {
		return
	}

	// Sanity check: skip if mid prices differ by > 5%
	priceDiff, _ := makerMid.Sub(takerMid).Abs().Div(midPrice).Float64()
	if priceDiff > 0.05 {
		e.logger.Warnw("price divergence too large, skipping",
			"makerMid", makerMid, "takerMid", takerMid, "diff", priceDiff)
		return
	}

	// Calculate spreads in BPS
	// AB: takerBid - makerAsk → if positive, can buy on maker, sell on taker
	spreadAB, _ := taker.BidPrice.Sub(maker.AskPrice).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()
	// BA: makerBid - takerAsk → if positive, can buy on taker, sell on maker
	spreadBA, _ := maker.BidPrice.Sub(taker.AskPrice).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()

	// Protect warmup tracking and stats with the engine mutex to avoid data races
	// between concurrent maker/taker WatchOrderBook goroutines.
	e.mu.Lock()
	if e.firstSeen.IsZero() {
		e.firstSeen = time.Now()
	}
	e.tickCount++

	// Feed rolling stats
	e.statsAB.Add(spreadAB)
	e.statsBA.Add(spreadBA)

	now := time.Now()
	tickCount := e.tickCount
	warmedUp := tickCount >= e.config.WarmupTicks &&
		now.Sub(e.firstSeen) >= e.config.WarmupDuration

	// Log warmup progress under lock (modifies e.warmupLogged)
	if !warmedUp {
		e.logWarmupMilestone(tickCount, now)
	}

	// Calculate Z-Scores and snapshot all stats under lock
	zAB := e.statsAB.ZScore(spreadAB)
	zBA := e.statsBA.ZScore(spreadBA)
	meanAB := e.statsAB.Mean()
	stddevAB := e.statsAB.StdDev()
	meanBA := e.statsBA.Mean()
	stddevBA := e.statsBA.StdDev()
	e.mu.Unlock()

	e.emitMarketUpdate()

	// Log periodic status (outside lock)
	if warmedUp && tickCount%200 == 0 {
		e.logger.Infof("📈 AB=%.1fbps(Z=%.2f) BA=%.1fbps(Z=%.2f) μ=%.2f σ=%.2f",
			spreadAB, zAB, spreadBA, zBA, meanAB, stddevAB)
	}

	// No signals before warmup.
	if !warmedUp {
		return
	}

	// Evaluate signals (outside lock, using snapshotted values)
	fees := e.RoundTripFeeBps()
	snapshot := sanitizeSnapshot(e.config, makerRaw, takerRaw, meanAB, meanBA, stddevAB, stddevBA, now)
	requiredOpenBps := fees + e.config.MinProfitBps + e.config.ImpactBufferBps + e.config.LatencyBufferBps

	// Check AB direction: Long maker, Short taker
	if snapshot.ValidAB {
		qty := snapshot.executableQuantity(e.config, LongMakerShortTaker)
		dynamicThreshold := meanAB + e.config.ZOpen*stddevAB
		if qty.GreaterThan(decimal.Zero) && spreadAB >= dynamicThreshold && spreadAB > requiredOpenBps {
			e.emitSignal(&Signal{
				Direction:           LongMakerShortTaker,
				SpreadBps:           spreadAB,
				ZScore:              zAB,
				Mean:                meanAB,
				StdDev:              stddevAB,
				ExpectedProfit:      spreadAB - fees,
				NetEdgeBps:          spreadAB - requiredOpenBps,
				DynamicThresholdBps: dynamicThreshold,
				Quantity:            qty,
				MakerBid:            snapshot.MakerBid,
				MakerAsk:            snapshot.MakerAsk,
				TakerBid:            snapshot.TakerBid,
				TakerAsk:            snapshot.TakerAsk,
				MakerBidQty:         snapshot.MakerBidQty,
				MakerAskQty:         snapshot.MakerAskQty,
				TakerBidQty:         snapshot.TakerBidQty,
				TakerAskQty:         snapshot.TakerAskQty,
				Timestamp:           now,
			})
		}
	}

	// Check BA direction: Long taker, Short maker
	if snapshot.ValidBA {
		qty := snapshot.executableQuantity(e.config, LongTakerShortMaker)
		dynamicThreshold := meanBA + e.config.ZOpen*stddevBA
		if qty.GreaterThan(decimal.Zero) && spreadBA >= dynamicThreshold && spreadBA > requiredOpenBps {
			e.emitSignal(&Signal{
				Direction:           LongTakerShortMaker,
				SpreadBps:           spreadBA,
				ZScore:              zBA,
				Mean:                meanBA,
				StdDev:              stddevBA,
				ExpectedProfit:      spreadBA - fees,
				NetEdgeBps:          spreadBA - requiredOpenBps,
				DynamicThresholdBps: dynamicThreshold,
				Quantity:            qty,
				MakerBid:            snapshot.MakerBid,
				MakerAsk:            snapshot.MakerAsk,
				TakerBid:            snapshot.TakerBid,
				TakerAsk:            snapshot.TakerAsk,
				MakerBidQty:         snapshot.MakerBidQty,
				MakerAskQty:         snapshot.MakerAskQty,
				TakerBidQty:         snapshot.TakerBidQty,
				TakerAskQty:         snapshot.TakerAskQty,
				Timestamp:           now,
			})
		}
	}
}

func (e *Engine) emitSignal(sig *Signal) {
	if e.onSignal != nil {
		e.onSignal(sig)
	}
}

func (e *Engine) emitMarketUpdate() {
	if e.onMarketUpdate != nil {
		e.onMarketUpdate()
	}
}

// Snapshot returns the latest top-of-book data and rolling means.
func (e *Engine) Snapshot() *Snapshot {
	if e == nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	meanAB := 0.0
	stddevAB := 0.0
	if e.statsAB != nil {
		meanAB = e.statsAB.Mean()
		stddevAB = e.statsAB.StdDev()
	}
	meanBA := 0.0
	stddevBA := 0.0
	if e.statsBA != nil {
		meanBA = e.statsBA.Mean()
		stddevBA = e.statsBA.StdDev()
	}
	now := time.Now()
	maker := TopOfBook{
		BidPrice:  e.makerBid,
		BidQty:    e.makerBidQty,
		AskPrice:  e.makerAsk,
		AskQty:    e.makerAskQty,
		Timestamp: e.makerTS,
	}
	taker := TopOfBook{
		BidPrice:  e.takerBid,
		BidQty:    e.takerBidQty,
		AskPrice:  e.takerAsk,
		AskQty:    e.takerAskQty,
		Timestamp: e.takerTS,
	}

	snapshot := sanitizeSnapshot(e.config, maker, taker, meanAB, meanBA, stddevAB, stddevBA, now)
	return &snapshot
}

// CurrentZ returns the current Z-Scores for both directions.
// Used by the trader to check close/stop-loss conditions.
func (e *Engine) CurrentZ() (zAB, zBA float64) {
	snapshot := e.Snapshot()
	if snapshot == nil {
		return 0, 0
	}
	if snapshot.StdDevAB > 1e-9 {
		zAB = (snapshot.SpreadAB - snapshot.MeanAB) / snapshot.StdDevAB
	}
	if snapshot.StdDevBA > 1e-9 {
		zBA = (snapshot.SpreadBA - snapshot.MeanBA) / snapshot.StdDevBA
	}
	return zAB, zBA
}

// IsWarmedUp returns whether the engine has enough data to emit signals.
func (e *Engine) IsWarmedUp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tickCount >= e.config.WarmupTicks &&
		time.Since(e.firstSeen) >= e.config.WarmupDuration
}

func orderBookTimestamp(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	if ts > 1_000_000_000_000 {
		return time.UnixMilli(ts)
	}
	return time.Unix(ts, 0)
}

func firstLevel(levels []marketdata.MarketLevel) (decimal.Decimal, decimal.Decimal) {
	if len(levels) == 0 {
		return decimal.Zero, decimal.Zero
	}
	return levels[0].Price, levels[0].Quantity
}

// logWarmupMilestone logs warmup progress at 25/50/75/100% milestones. Must be called under e.mu.
func (e *Engine) logWarmupMilestone(tickCount int, now time.Time) {
	elapsed := now.Sub(e.firstSeen).Round(time.Second)
	target := e.config.WarmupTicks
	durationMet := elapsed >= e.config.WarmupDuration

	type milestone struct {
		pct  int
		mask int
	}
	for _, m := range []milestone{{25, 1}, {50, 2}, {75, 4}, {100, 8}} {
		threshold := target * m.pct / 100
		if tickCount >= threshold && e.warmupLogged&m.mask == 0 {
			e.warmupLogged |= m.mask
			if m.pct < 100 {
				e.logger.Infof("⏳ warmup %d%% (%d/%d ticks, %s)", m.pct, tickCount, target, elapsed)
			} else if !durationMet {
				e.logger.Infof("⏳ warmup ticks ready (%d/%d) — waiting for %s duration", tickCount, target, e.config.WarmupDuration)
			}
		}
	}

	if durationMet && tickCount >= target && e.warmupLogged&16 == 0 {
		e.warmupLogged |= 16
		e.logger.Infof("✅ warmup complete (%d ticks, %s)", tickCount, elapsed)
	}
}
