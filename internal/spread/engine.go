package spread

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"sync"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// Engine monitors BBO from two exchanges and detects arbitrage signals.
type Engine struct {
	maker  exchanges.Exchange
	taker  exchanges.Exchange
	symbol string
	config *appconfig.Config
	logger *zap.SugaredLogger

	mu       sync.Mutex
	makerBid decimal.Decimal
	makerAsk decimal.Decimal
	takerBid decimal.Decimal
	takerAsk decimal.Decimal

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

	// CSV writer for observe-only mode
	csvWriter *csv.Writer
	csvFile   *os.File
}

// New creates a new spread engine.
func New(maker, taker exchanges.Exchange, cfg *appconfig.Config, logger *zap.SugaredLogger) *Engine {
	return &Engine{
		maker:   maker,
		taker:   taker,
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

// Start begins monitoring spread via WatchOrderBook on both exchanges.
func (e *Engine) Start(ctx context.Context) error {
	// Setup CSV writer for observe-only mode
	if e.config.ObserveOnly {
		filename := fmt.Sprintf("spread_%s_%s_%s_%s.csv",
			e.config.MakerExchange, e.config.TakerExchange, e.symbol,
			time.Now().Format("20060102_150405"))
		f, err := os.Create(filename)
		if err != nil {
			return fmt.Errorf("create CSV: %w", err)
		}
		e.csvFile = f
		e.csvWriter = csv.NewWriter(f)
		e.csvWriter.Write([]string{
			"timestamp", "maker_bid", "maker_ask", "taker_bid", "taker_ask",
			"spread_ab_bps", "spread_ba_bps",
			"mean_ab", "stddev_ab", "z_ab",
			"mean_ba", "stddev_ba", "z_ba",
		})
		e.logger.Infow("observe-only mode: writing to CSV", "file", filename)
	}

	// Subscribe to orderbooks
	errCh := make(chan error, 2)

	go func() {
		err := e.maker.WatchOrderBook(ctx, e.symbol, func(ob *exchanges.OrderBook) {
			if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
				return
			}
			e.mu.Lock()
			e.makerBid = ob.Bids[0].Price
			e.makerAsk = ob.Asks[0].Price
			e.mu.Unlock()
			e.onBBOUpdate()
		})
		if err != nil {
			errCh <- fmt.Errorf("maker WatchOrderBook: %w", err)
		}
	}()

	go func() {
		err := e.taker.WatchOrderBook(ctx, e.symbol, func(ob *exchanges.OrderBook) {
			if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
				return
			}
			e.mu.Lock()
			e.takerBid = ob.Bids[0].Price
			e.takerAsk = ob.Asks[0].Price
			e.mu.Unlock()
			e.onBBOUpdate()
		})
		if err != nil {
			errCh <- fmt.Errorf("taker WatchOrderBook: %w", err)
		}
	}()

	// Wait for first error or context done
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close cleans up resources.
func (e *Engine) Close() {
	if e.csvWriter != nil {
		e.csvWriter.Flush()
	}
	if e.csvFile != nil {
		e.csvFile.Close()
	}
}

// onBBOUpdate is called whenever either exchange's BBO updates.
// It is called concurrently from maker and taker WatchOrderBook goroutines.
func (e *Engine) onBBOUpdate() {
	e.mu.Lock()
	makerBid := e.makerBid
	makerAsk := e.makerAsk
	takerBid := e.takerBid
	takerAsk := e.takerAsk
	e.mu.Unlock()

	// Need all four prices
	if makerBid.IsZero() || makerAsk.IsZero() || takerBid.IsZero() || takerAsk.IsZero() {
		return
	}

	// Calculate mid price for normalization
	makerMid := makerBid.Add(makerAsk).Div(decimal.NewFromInt(2))
	takerMid := takerBid.Add(takerAsk).Div(decimal.NewFromInt(2))
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
	spreadAB, _ := takerBid.Sub(makerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()
	// BA: makerBid - takerAsk → if positive, can buy on taker, sell on maker
	spreadBA, _ := makerBid.Sub(takerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()

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

	// CSV output for observe-only mode (outside lock)
	e.emitMarketUpdate()
	if e.csvWriter != nil {
		if err := e.csvWriter.Write([]string{
			now.Format(time.RFC3339Nano),
			makerBid.String(), makerAsk.String(),
			takerBid.String(), takerAsk.String(),
			fmt.Sprintf("%.4f", spreadAB), fmt.Sprintf("%.4f", spreadBA),
			fmt.Sprintf("%.4f", meanAB), fmt.Sprintf("%.4f", stddevAB), fmt.Sprintf("%.4f", zAB),
			fmt.Sprintf("%.4f", meanBA), fmt.Sprintf("%.4f", stddevBA), fmt.Sprintf("%.4f", zBA),
		}); err != nil {
			e.logger.Warnw("CSV write failed", "err", err)
		}
		// Flush CSV periodically to prevent data loss
		if tickCount%100 == 0 {
			e.csvWriter.Flush()
		}
	}

	// Log periodic status (outside lock)
	if warmedUp && tickCount%200 == 0 {
		e.logger.Infof("📈 AB=%.1fbps(Z=%.2f) BA=%.1fbps(Z=%.2f) μ=%.2f σ=%.2f",
			spreadAB, zAB, spreadBA, zBA, meanAB, stddevAB)
	}

	// No signals before warmup or in observe-only mode
	if !warmedUp || e.config.ObserveOnly {
		return
	}

	// Evaluate signals (outside lock, using snapshotted values)
	fees := e.RoundTripFeeBps()

	// Check AB direction: Long maker, Short taker
	if zAB > e.config.ZOpen {
		expectedProfit := spreadAB - meanAB
		if expectedProfit > fees+e.config.MinProfitBps {
			e.emitSignal(&Signal{
				Direction:      LongMakerShortTaker,
				SpreadBps:      spreadAB,
				ZScore:         zAB,
				Mean:           meanAB,
				StdDev:         stddevAB,
				ExpectedProfit: expectedProfit - fees,
				MakerBid:       makerBid,
				MakerAsk:       makerAsk,
				TakerBid:       takerBid,
				TakerAsk:       takerAsk,
				Timestamp:      now,
			})
		}
	}

	// Check BA direction: Long taker, Short maker
	if zBA > e.config.ZOpen {
		expectedProfit := spreadBA - meanBA
		if expectedProfit > fees+e.config.MinProfitBps {
			e.emitSignal(&Signal{
				Direction:      LongTakerShortMaker,
				SpreadBps:      spreadBA,
				ZScore:         zBA,
				Mean:           meanBA,
				StdDev:         stddevBA,
				ExpectedProfit: expectedProfit - fees,
				MakerBid:       makerBid,
				MakerAsk:       makerAsk,
				TakerBid:       takerBid,
				TakerAsk:       takerAsk,
				Timestamp:      now,
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
	if e.statsAB != nil {
		meanAB = e.statsAB.Mean()
	}
	meanBA := 0.0
	if e.statsBA != nil {
		meanBA = e.statsBA.Mean()
	}

	return &Snapshot{
		MakerBid: e.makerBid,
		MakerAsk: e.makerAsk,
		TakerBid: e.takerBid,
		TakerAsk: e.takerAsk,
		MeanAB:   meanAB,
		MeanBA:   meanBA,
	}
}

// CurrentZ returns the current Z-Scores for both directions.
// Used by the trader to check close/stop-loss conditions.
func (e *Engine) CurrentZ() (zAB, zBA float64) {
	e.mu.Lock()
	makerBid := e.makerBid
	makerAsk := e.makerAsk
	takerBid := e.takerBid
	takerAsk := e.takerAsk

	if makerBid.IsZero() || takerBid.IsZero() {
		e.mu.Unlock()
		return 0, 0
	}

	makerMid := makerBid.Add(makerAsk).Div(decimal.NewFromInt(2))
	takerMid := takerBid.Add(takerAsk).Div(decimal.NewFromInt(2))
	midPrice := makerMid.Add(takerMid).Div(decimal.NewFromInt(2))

	if midPrice.IsZero() {
		e.mu.Unlock()
		return 0, 0
	}

	spreadABVal, _ := takerBid.Sub(makerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()
	spreadBAVal, _ := makerBid.Sub(takerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()

	// Access stats under lock to prevent concurrent read/write with onBBOUpdate.
	zABResult := e.statsAB.ZScore(spreadABVal)
	zBAResult := e.statsBA.ZScore(spreadBAVal)
	e.mu.Unlock()

	return zABResult, zBAResult
}

// IsWarmedUp returns whether the engine has enough data to emit signals.
func (e *Engine) IsWarmedUp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tickCount >= e.config.WarmupTicks &&
		time.Since(e.firstSeen) >= e.config.WarmupDuration
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
