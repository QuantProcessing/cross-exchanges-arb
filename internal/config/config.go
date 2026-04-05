package config

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
	WindowSize           int           // rolling window size in ticks (default: 500)
	ZOpen                float64       // Z-Score threshold to open position (default: 2.0)
	ZClose               float64       // Z-Score threshold to close position (default: 0.5)
	ZStop                float64       // Z-Score threshold for stop-loss (default: -1.0)
	MinProfitBps         float64       // minimum net profit in BPS after fees (default: 1.0)
	ImpactBufferBps      float64       // extra bps buffer for market impact
	LatencyBufferBps     float64       // extra bps buffer for data/execution latency
	LiquidityBufferRatio float64       // buffer ratio applied to the thinner top-level side
	WarmupTicks          int           // minimum ticks before signals are emitted (default: 200)
	WarmupDuration       time.Duration // minimum time before signals are emitted (default: 3m)

	// Safety params
	Cooldown     time.Duration // cooldown between trades (default: 5s)
	MaxHoldTime  time.Duration // max position hold time before force close (default: 30m)
	Slippage     float64       // slippage tolerance for market orders (default: 0.002 = 0.2%)
	MakerTimeout time.Duration // maker order timeout before cancel/reset
	MaxQuoteAge  time.Duration // optional freshness window for top-of-book quotes
	MaxRounds    int           // max completed rounds in live validation mode
}

// Parse parses CLI flags and returns a Config.
func Parse() *Config {
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
	flag.Float64Var(&c.MinProfitBps, "min-profit", 10.0, "Minimum net profit BPS after fees")
	flag.Float64Var(&c.ImpactBufferBps, "impact-buffer-bps", 0, "Additional bps buffer for market impact")
	flag.Float64Var(&c.LatencyBufferBps, "latency-buffer-bps", 0, "Additional bps buffer for latency/drift")
	flag.Float64Var(&c.LiquidityBufferRatio, "liquidity-buffer-ratio", 0.8, "Buffer ratio applied to thinner top-level liquidity when sizing orders")
	flag.IntVar(&c.WarmupTicks, "warmup-ticks", 200, "Minimum ticks before trading")
	warmupDur := flag.String("warmup-duration", "3m", "Minimum duration before trading")

	// Safety
	cooldown := flag.String("cooldown", "5s", "Cooldown between trades")
	maxHold := flag.String("max-hold", "30m", "Max position hold time")
	flag.Float64Var(&c.Slippage, "slippage", 0.002, "Slippage tolerance for market orders")
	flag.DurationVar(&c.MakerTimeout, "maker-timeout", 15*time.Second, "Maker order timeout before cancel/reset")
	flag.DurationVar(&c.MaxQuoteAge, "max-quote-age", 1500*time.Millisecond, "Maximum acceptable top-of-book quote age")
	flag.IntVar(&c.MaxRounds, "max-rounds", 1, "Maximum completed rounds in validation mode")

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

	// Validate config
	if err := c.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}

	return c
}

// Validate checks if the config is valid.
func (c *Config) Validate() error {
	if c.Quantity.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("quantity must be positive, got %s", c.Quantity)
	}
	if c.WindowSize < 10 {
		return fmt.Errorf("window size must be >= 10, got %d", c.WindowSize)
	}
	if c.ZClose >= c.ZOpen {
		return fmt.Errorf("z-close (%.2f) must be < z-open (%.2f)", c.ZClose, c.ZOpen)
	}
	if c.ZStop >= c.ZClose {
		return fmt.Errorf("z-stop (%.2f) must be < z-close (%.2f)", c.ZStop, c.ZClose)
	}
	if c.Slippage < 0 || c.Slippage > 0.1 {
		return fmt.Errorf("slippage must be between 0 and 0.1, got %.4f", c.Slippage)
	}
	if c.LiquidityBufferRatio <= 0 || c.LiquidityBufferRatio > 1 {
		return fmt.Errorf("liquidity-buffer-ratio must be in (0, 1], got %.4f", c.LiquidityBufferRatio)
	}
	if c.MakerTimeout < time.Second {
		return fmt.Errorf("maker-timeout must be >= 1s, got %s", c.MakerTimeout)
	}
	if c.MaxQuoteAge < 0 {
		return fmt.Errorf("max-quote-age must be >= 0, got %s", c.MaxQuoteAge)
	}
	if c.MaxRounds < 1 {
		return fmt.Errorf("max-rounds must be >= 1, got %d", c.MaxRounds)
	}
	return nil
}

// String returns a formatted summary of the config for the startup dashboard.
func (c *Config) String() string {
	return fmt.Sprintf(`
════════════════════════════════════════════════════════
  Cross-Exchange Spread Arbitrage
  Maker: %-12s  Taker: %-12s
  Symbol: %-10s    Qty: %s
────────────────────────────────────────────────────────
  Z-Open: %.1f      Z-Close: %.1f     Z-Stop: %.1f
  Window: %d ticks   MinProfit: %.1f BPS
  Warmup: %d ticks / %s
  Cooldown: %s      MaxHold: %s     Slippage: %.2f%%
  ImpactBuf: %.1fbps LatencyBuf: %.1fbps LiqBuf: %.0f%%
  MakerTimeout: %-8s QuoteAge: %-8s MaxRounds: %d
	════════════════════════════════════════════════════════`,
		c.MakerExchange, c.TakerExchange,
		c.Symbol, c.Quantity.String(),
		c.ZOpen, c.ZClose, c.ZStop,
		c.WindowSize, c.MinProfitBps,
		c.WarmupTicks, c.WarmupDuration,
		c.Cooldown, c.MaxHoldTime, c.Slippage*100,
		c.ImpactBufferBps, c.LatencyBufferBps, c.LiquidityBufferRatio*100,
		c.MakerTimeout, c.MaxQuoteAge, c.MaxRounds,
	)
}
