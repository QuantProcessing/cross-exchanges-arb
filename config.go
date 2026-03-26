package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Config holds all runtime configuration parsed from CLI flags.
type Config struct {
	// Exchange pair
	MakerExchange      string // exchange to place maker orders (e.g. "DECIBEL")
	TakerExchange      string // exchange to hedge/taker orders (e.g. "LIGHTER")
	MakerQuoteCurrency string // optional maker quote currency override
	TakerQuoteCurrency string // optional taker quote currency override

	// Trading params
	Symbol   string          // base currency symbol, e.g. "BTC"
	Quantity decimal.Decimal // order quantity in base currency

	// Algorithm params (Z-Score)
	WindowSize     int           // rolling window size in ticks (default: 500)
	ZOpen          float64       // Z-Score threshold to open position (default: 2.0)
	ZClose         float64       // Z-Score threshold to close position (default: 0.5)
	ZStop          float64       // Z-Score threshold for stop-loss (default: -1.0)
	MinProfitBps   float64       // minimum net profit in BPS after fees (default: 1.0)
	WarmupTicks    int           // minimum ticks before signals are emitted (default: 200)
	WarmupDuration time.Duration // minimum time before signals are emitted (default: 3m)

	// Safety params
	Cooldown     time.Duration // cooldown between trades (default: 5s)
	MaxHoldTime  time.Duration // max position hold time before force close (default: 30m)
	Slippage     float64       // slippage tolerance for market orders (default: 0.002 = 0.2%)
	MakerTimeout time.Duration // maker order timeout before cancel/reset
	MaxRounds    int           // max completed rounds in validation mode
	LiveValidate bool          // live validation mode toggle

	// Run modes
	DryRun      bool // simulate only, no real orders
	ObserveOnly bool // Phase 0: only collect data, output CSV
}

// ParseConfig parses CLI flags and returns a Config.
func ParseConfig() *Config {
	c := &Config{}

	// Exchange pair
	flag.StringVar(&c.MakerExchange, "maker", "EDGEX", "Maker exchange name")
	flag.StringVar(&c.TakerExchange, "taker", "LIGHTER", "Taker/hedge exchange name")
	flag.StringVar(&c.MakerQuoteCurrency, "maker-quote-currency", "", "Optional maker quote currency override")
	flag.StringVar(&c.TakerQuoteCurrency, "taker-quote-currency", "", "Optional taker quote currency override")

	// Trading
	flag.StringVar(&c.Symbol, "symbol", "BTC", "Trading symbol (base currency)")
	qty := flag.String("qty", "0.001", "Order quantity in base currency")

	// Algorithm
	flag.IntVar(&c.WindowSize, "window", 500, "Rolling window size in ticks")
	flag.Float64Var(&c.ZOpen, "z-open", 2.0, "Z-Score threshold to open position")
	flag.Float64Var(&c.ZClose, "z-close", 0.5, "Z-Score threshold to close position")
	flag.Float64Var(&c.ZStop, "z-stop", -1.0, "Z-Score for stop-loss (negative = spread reversed)")
	flag.Float64Var(&c.MinProfitBps, "min-profit", 1.0, "Minimum net profit BPS after fees")
	flag.IntVar(&c.WarmupTicks, "warmup-ticks", 200, "Minimum ticks before trading")
	warmupDur := flag.String("warmup-duration", "3m", "Minimum duration before trading")

	// Safety
	cooldown := flag.String("cooldown", "5s", "Cooldown between trades")
	maxHold := flag.String("max-hold", "30m", "Max position hold time")
	flag.Float64Var(&c.Slippage, "slippage", 0.002, "Slippage tolerance for market orders")
	flag.DurationVar(&c.MakerTimeout, "maker-timeout", 15*time.Second, "Maker order timeout before cancel/reset")
	flag.IntVar(&c.MaxRounds, "max-rounds", 1, "Maximum completed rounds in validation mode")
	flag.BoolVar(&c.LiveValidate, "live-validate", true, "Enable live validation mode")

	// Run modes
	flag.BoolVar(&c.DryRun, "dry-run", false, "Simulate only, no real orders")
	flag.BoolVar(&c.ObserveOnly, "observe-only", false, "Phase 0: only collect spread data to CSV")

	flag.Parse()

	// Parse quantity
	var err error
	c.Quantity, err = decimal.NewFromString(*qty)
	if err != nil {
		panic(fmt.Sprintf("invalid quantity %q: %v", *qty, err))
	}

	// Parse durations
	c.WarmupDuration, err = time.ParseDuration(*warmupDur)
	if err != nil {
		panic(fmt.Sprintf("invalid warmup-duration %q: %v", *warmupDur, err))
	}
	c.Cooldown, err = time.ParseDuration(*cooldown)
	if err != nil {
		panic(fmt.Sprintf("invalid cooldown %q: %v", *cooldown, err))
	}
	c.MaxHoldTime, err = time.ParseDuration(*maxHold)
	if err != nil {
		panic(fmt.Sprintf("invalid max-hold %q: %v", *maxHold, err))
	}

	return c
}

// String returns a formatted summary of the config for the startup dashboard.
func (c *Config) String() string {
	mode := "LIVE"
	switch {
	case c.ObserveOnly:
		mode = "OBSERVE ONLY"
	case c.DryRun:
		mode = "DRY RUN"
	}

	return fmt.Sprintf(`
════════════════════════════════════════════════════════
  Cross-Exchange Spread Arbitrage
  Maker: %-12s  Taker: %-12s
  Symbol: %-10s    Qty: %s    Mode: %s
────────────────────────────────────────────────────────
  Z-Open: %.1f      Z-Close: %.1f     Z-Stop: %.1f
  Window: %d ticks   MinProfit: %.1f BPS
  Warmup: %d ticks / %s
  Cooldown: %s      MaxHold: %s     Slippage: %.2f%%
  ValidationCfg: %-8s MakerTimeout: %-8s MaxRounds: %d
════════════════════════════════════════════════════════`,
		c.MakerExchange, c.TakerExchange,
		c.Symbol, c.Quantity.String(), mode,
		c.ZOpen, c.ZClose, c.ZStop,
		c.WindowSize, c.MinProfitBps,
		c.WarmupTicks, c.WarmupDuration,
		c.Cooldown, c.MaxHoldTime, c.Slippage*100,
		func() string {
			if c.LiveValidate {
				return "ENABLED"
			}
			return "DISABLED"
		}(), c.MakerTimeout, c.MaxRounds,
	)
}
