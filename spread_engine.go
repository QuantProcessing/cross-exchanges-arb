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

// --- Spread Statistics (Rolling Window) ---

// SpreadStats maintains rolling statistics (mean, stddev) over a fixed window.
type SpreadStats struct {
	window  []float64
	maxSize int
	sum     float64
	sumSq   float64
}

// NewSpreadStats creates a new stats tracker with the given window size.
func NewSpreadStats(windowSize int) *SpreadStats {
	return &SpreadStats{
		window:  make([]float64, 0, windowSize),
		maxSize: windowSize,
	}
}

// Add inserts a new spread value and maintains the rolling window.
func (s *SpreadStats) Add(spread float64) {
	if len(s.window) >= s.maxSize {
		// Remove oldest
		old := s.window[0]
		s.window = s.window[1:]
		s.sum -= old
		s.sumSq -= old * old
	}
	s.window = append(s.window, spread)
	s.sum += spread
	s.sumSq += spread * spread
}

// Count returns the number of samples in the window.
func (s *SpreadStats) Count() int {
	return len(s.window)
}

// Mean returns the rolling mean.
func (s *SpreadStats) Mean() float64 {
	n := len(s.window)
	if n == 0 {
		return 0
	}
	return s.sum / float64(n)
}

// StdDev returns the rolling standard deviation.
func (s *SpreadStats) StdDev() float64 {
	n := len(s.window)
	if n < 2 {
		return 0
	}
	mean := s.sum / float64(n)
	variance := s.sumSq/float64(n) - mean*mean
	if variance < 0 {
		variance = 0 // numerical safety
	}
	return math.Sqrt(variance)
}

// ZScore returns the Z-Score of a given spread value relative to the rolling distribution.
func (s *SpreadStats) ZScore(spread float64) float64 {
	sd := s.StdDev()
	if sd < 1e-9 {
		return 0
	}
	return (spread - s.Mean()) / sd
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
	firstSeen time.Time
	tickCount int

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

	// Track warmup
	if e.firstSeen.IsZero() {
		e.firstSeen = time.Now()
	}
	e.tickCount++

	// Feed rolling stats
	e.statsAB.Add(spreadAB)
	e.statsBA.Add(spreadBA)

	now := time.Now()
	warmedUp := e.tickCount >= e.config.WarmupTicks &&
		now.Sub(e.firstSeen) >= e.config.WarmupDuration

	// Calculate Z-Scores
	zAB := e.statsAB.ZScore(spreadAB)
	zBA := e.statsBA.ZScore(spreadBA)

	// CSV output for observe-only mode
	if e.csvWriter != nil {
		e.csvWriter.Write([]string{
			now.Format(time.RFC3339Nano),
			makerBid.String(), makerAsk.String(),
			takerBid.String(), takerAsk.String(),
			fmt.Sprintf("%.4f", spreadAB), fmt.Sprintf("%.4f", spreadBA),
			fmt.Sprintf("%.4f", e.statsAB.Mean()), fmt.Sprintf("%.4f", e.statsAB.StdDev()), fmt.Sprintf("%.4f", zAB),
			fmt.Sprintf("%.4f", e.statsBA.Mean()), fmt.Sprintf("%.4f", e.statsBA.StdDev()), fmt.Sprintf("%.4f", zBA),
		})
	}

	// Log periodic status
	if e.tickCount%50 == 0 {
		if !warmedUp {
			e.logger.Infow("warming up",
				"ticks", fmt.Sprintf("%d/%d", e.tickCount, e.config.WarmupTicks),
				"elapsed", now.Sub(e.firstSeen).Round(time.Second),
			)
		} else {
			e.logger.Infow("spread status",
				"AB", fmt.Sprintf("%.2f bps (Z=%.2f)", spreadAB, zAB),
				"BA", fmt.Sprintf("%.2f bps (Z=%.2f)", spreadBA, zBA),
				"μ_AB", fmt.Sprintf("%.2f", e.statsAB.Mean()),
				"σ_AB", fmt.Sprintf("%.2f", e.statsAB.StdDev()),
			)
		}
	}

	// No signals before warmup or in observe-only mode
	if !warmedUp || e.config.ObserveOnly {
		return
	}

	// Evaluate signals
	fees := e.roundTripFeeBps()

	// Check AB direction: Long maker, Short taker
	if zAB > e.config.ZOpen {
		expectedProfit := spreadAB - e.statsAB.Mean()
		if expectedProfit > fees+e.config.MinProfitBps {
			e.emitSignal(&SpreadSignal{
				Direction:      LongMakerShortTaker,
				SpreadBps:      spreadAB,
				ZScore:         zAB,
				Mean:           e.statsAB.Mean(),
				StdDev:         e.statsAB.StdDev(),
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
		expectedProfit := spreadBA - e.statsBA.Mean()
		if expectedProfit > fees+e.config.MinProfitBps {
			e.emitSignal(&SpreadSignal{
				Direction:      LongTakerShortMaker,
				SpreadBps:      spreadBA,
				ZScore:         zBA,
				Mean:           e.statsBA.Mean(),
				StdDev:         e.statsBA.StdDev(),
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
	e.mu.Unlock()

	if makerBid.IsZero() || takerBid.IsZero() {
		return 0, 0
	}

	makerMid := makerBid.Add(makerAsk).Div(decimal.NewFromInt(2))
	takerMid := takerBid.Add(takerAsk).Div(decimal.NewFromInt(2))
	midPrice := makerMid.Add(takerMid).Div(decimal.NewFromInt(2))

	if midPrice.IsZero() {
		return 0, 0
	}

	spreadABVal, _ := takerBid.Sub(makerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()
	spreadBAVal, _ := makerBid.Sub(takerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()

	return e.statsAB.ZScore(spreadABVal), e.statsBA.ZScore(spreadBAVal)
}

// IsWarmedUp returns whether the engine has enough data to emit signals.
func (e *SpreadEngine) IsWarmedUp() bool {
	return e.tickCount >= e.config.WarmupTicks &&
		time.Since(e.firstSeen) >= e.config.WarmupDuration
}
