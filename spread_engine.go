package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// --- Spread Statistics (Rolling Window / Ring Buffer) ---

// SpreadStats maintains rolling statistics (mean, stddev) over a fixed-size ring buffer.
// The ring buffer prevents memory leaks from slice reslicing.
type SpreadStats struct {
	buf     []float64
	maxSize int
	count   int // total items written (capped at maxSize)
	writeAt int // next write position
	mean    float64
	stddev  float64
	dirty   bool
}

// NewSpreadStats creates a new stats tracker with the given window size.
func NewSpreadStats(windowSize int) *SpreadStats {
	if windowSize < 1 {
		windowSize = 1
	}
	return &SpreadStats{
		buf:     make([]float64, windowSize),
		maxSize: windowSize,
		dirty:   true,
	}
}

// Add inserts a new spread value into the ring buffer.
func (s *SpreadStats) Add(spread float64) {
	s.buf[s.writeAt] = spread
	s.writeAt = (s.writeAt + 1) % s.maxSize
	if s.count < s.maxSize {
		s.count++
	}
	s.dirty = true
}

// Count returns the number of samples in the window.
func (s *SpreadStats) Count() int {
	return s.count
}

// Mean returns the rolling mean.
func (s *SpreadStats) Mean() float64 {
	s.recomputeIfDirty()
	return s.mean
}

// StdDev returns the rolling standard deviation.
func (s *SpreadStats) StdDev() float64 {
	s.recomputeIfDirty()
	return s.stddev
}

// ZScore returns the Z-Score of a given spread value relative to the rolling distribution.
func (s *SpreadStats) ZScore(spread float64) float64 {
	sd := s.StdDev()
	if sd < 1e-9 {
		return 0
	}
	return (spread - s.Mean()) / sd
}

// activeSlice returns the current valid data in insertion order.
func (s *SpreadStats) activeSlice() []float64 {
	if s.count < s.maxSize {
		return s.buf[:s.count]
	}
	// Ring buffer is full: writeAt points to the oldest element.
	result := make([]float64, s.maxSize)
	copy(result, s.buf[s.writeAt:])
	copy(result[s.maxSize-s.writeAt:], s.buf[:s.writeAt])
	return result
}

func (s *SpreadStats) recomputeIfDirty() {
	if !s.dirty {
		return
	}

	data := s.activeSlice()
	if len(data) == 0 {
		s.mean = 0
		s.stddev = 0
		s.dirty = false
		return
	}

	// Welford's online algorithm for numerically stable single-pass computation.
	mean := 0.0
	m2 := 0.0
	for i, sample := range data {
		delta := sample - mean
		mean += delta / float64(i+1)
		delta2 := sample - mean
		m2 += delta * delta2
	}

	s.mean = mean
	if len(data) < 2 {
		s.stddev = 0
		s.dirty = false
		return
	}

	variance := m2 / float64(len(data))
	if variance < 0 {
		variance = 0
	}
	s.stddev = math.Sqrt(variance)
	s.dirty = false
}

// --- Spread Signal ---

// SpreadDirection indicates which direction to trade.
type SpreadDirection string

const (
	// LongMakerShortTaker: buy on maker exchange, sell on taker exchange.
	LongMakerShortTaker SpreadDirection = "LONG_MAKER_SHORT_TAKER"
	// LongTakerShortMaker: buy on taker exchange, sell on maker exchange.
	LongTakerShortMaker SpreadDirection = "LONG_TAKER_SHORT_MAKER"
)

// SpreadSignal represents a detected arbitrage opportunity.
type SpreadSignal struct {
	Direction       SpreadDirection
	SpreadBps       float64         // current spread in BPS
	ZScore          float64         // Z-Score of the spread
	Mean            float64         // current rolling mean (BPS)
	StdDev          float64         // current rolling stddev (BPS)
	ExpectedProfit  float64         // expected profit BPS (spread recovery to mean minus fees)
	MakerBid        decimal.Decimal // maker exchange best bid
	MakerAsk        decimal.Decimal // maker exchange best ask
	TakerBid        decimal.Decimal // taker exchange best bid
	TakerAsk        decimal.Decimal // taker exchange best ask
	Timestamp       time.Time
}

// --- Spread Engine ---

// SpreadEngine monitors BBO from two exchanges and detects arbitrage signals.
type SpreadEngine struct {
	maker  exchanges.Exchange
	taker  exchanges.Exchange
	symbol string
	config *Config
	logger *zap.SugaredLogger

	mu       sync.Mutex
	makerBid decimal.Decimal
	makerAsk decimal.Decimal
	takerBid decimal.Decimal
	takerAsk decimal.Decimal

	// Rolling stats for both directions
	statsAB *SpreadStats // spread: takerBid - makerAsk (Long Maker, Short Taker)
	statsBA *SpreadStats // spread: makerBid - takerAsk (Long Taker, Short Maker)

	// Warmup tracking
	firstSeen    time.Time
	tickCount    int
	warmupLogged int // bitmask: 1=25%, 2=50%, 4=75%, 8=100%, 16=complete

	// Fee rates (loaded at startup)
	makerFee FeeInfo
	takerFee FeeInfo

	// Signal callback
	onSignal func(signal *SpreadSignal)

	// CSV writer for observe-only mode
	csvWriter *csv.Writer
	csvFile   *os.File
}

// FeeInfo holds maker/taker fee rates for an exchange.
type FeeInfo struct {
	MakerRate float64 // e.g. 0.0001 = 1 BPS
	TakerRate float64 // e.g. 0.0004 = 4 BPS
}

// NewSpreadEngine creates a new spread engine.
func NewSpreadEngine(maker, taker exchanges.Exchange, cfg *Config, logger *zap.SugaredLogger) *SpreadEngine {
	return &SpreadEngine{
		maker:   maker,
		taker:   taker,
		symbol:  cfg.Symbol,
		config:  cfg,
		logger:  logger,
		statsAB: NewSpreadStats(cfg.WindowSize),
		statsBA: NewSpreadStats(cfg.WindowSize),
	}
}

// SetFees configures the fee rates for both exchanges.
func (e *SpreadEngine) SetFees(makerFee, takerFee FeeInfo) {
	e.makerFee = makerFee
	e.takerFee = takerFee
}

// SetSignalCallback sets the callback for when a signal is detected.
func (e *SpreadEngine) SetSignalCallback(cb func(signal *SpreadSignal)) {
	e.onSignal = cb
}

// roundTripFeeBps calculates the total round-trip fee in BPS.
// A round-trip involves 4 fee events: open on A + open on B + close on A + close on B.
// Maker exchange pays maker fees, taker exchange pays taker fees.
func (e *SpreadEngine) roundTripFeeBps() float64 {
	return (e.makerFee.MakerRate + e.takerFee.TakerRate) * 2 * 10000
}

// Start begins monitoring spread via WatchOrderBook on both exchanges.
func (e *SpreadEngine) Start(ctx context.Context) error {
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
func (e *SpreadEngine) Close() {
	if e.csvWriter != nil {
		e.csvWriter.Flush()
	}
	if e.csvFile != nil {
		e.csvFile.Close()
	}
}

// onBBOUpdate is called whenever either exchange's BBO updates.
// It is called concurrently from maker and taker WatchOrderBook goroutines.
func (e *SpreadEngine) onBBOUpdate() {
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

	// Sanity check: skip if mid prices differ by > 50%
	priceDiff, _ := makerMid.Sub(takerMid).Abs().Div(midPrice).Float64()
	if priceDiff > 0.5 {
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

	// Calculate Z-Scores
	zAB := e.statsAB.ZScore(spreadAB)
	zBA := e.statsBA.ZScore(spreadBA)

	// Snapshot stats values while still under lock
	meanAB := e.statsAB.Mean()
	stddevAB := e.statsAB.StdDev()
	meanBA := e.statsBA.Mean()
	stddevBA := e.statsBA.StdDev()
	e.mu.Unlock()

	// CSV output for observe-only mode
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
	}

	// Log periodic status
	if !warmedUp {
		e.logWarmupMilestone(tickCount, now)
	} else if tickCount%200 == 0 {
		e.logger.Infof("📈 AB=%.1fbps(Z=%.2f) BA=%.1fbps(Z=%.2f) μ=%.2f σ=%.2f",
			spreadAB, zAB, spreadBA, zBA, meanAB, stddevAB)
	}

	// No signals before warmup or in observe-only mode
	if !warmedUp || e.config.ObserveOnly {
		return
	}

	// Evaluate signals (use snapshotted stats values from above)
	fees := e.roundTripFeeBps()

	// Check AB direction: Long maker, Short taker
	if zAB > e.config.ZOpen {
		expectedProfit := spreadAB - meanAB
		if expectedProfit > fees+e.config.MinProfitBps {
			e.emitSignal(&SpreadSignal{
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
			e.emitSignal(&SpreadSignal{
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

func (e *SpreadEngine) emitSignal(sig *SpreadSignal) {
	if e.onSignal != nil {
		e.onSignal(sig)
	}
}

// GetCurrentZ returns the current Z-Scores for both directions.
// Used by the trader to check close/stop-loss conditions.
func (e *SpreadEngine) GetCurrentZ() (zAB, zBA float64) {
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
func (e *SpreadEngine) IsWarmedUp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tickCount >= e.config.WarmupTicks &&
		time.Since(e.firstSeen) >= e.config.WarmupDuration
}

// logWarmupMilestone logs warmup progress at 25/50/75/100% milestones. Must be called under e.mu.
func (e *SpreadEngine) logWarmupMilestone(tickCount int, now time.Time) {
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
