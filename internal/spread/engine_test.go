package spread

import (
	"math"
	"testing"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

func TestSpreadStats_RollingWindowTracksCurrentSamples(t *testing.T) {
	stats := NewStats(3)
	samples := []float64{1, 2, 3, 4, 5}

	for _, sample := range samples {
		stats.Add(sample)
	}

	wantWindow := []float64{3, 4, 5}
	if stats.Count() != len(wantWindow) {
		t.Fatalf("count = %d, want %d", stats.Count(), len(wantWindow))
	}

	wantMean, wantStdDev := stableStats(wantWindow)
	if math.Abs(stats.Mean()-wantMean) > 1e-12 {
		t.Fatalf("mean = %.12f, want %.12f", stats.Mean(), wantMean)
	}
	if math.Abs(stats.StdDev()-wantStdDev) > 1e-12 {
		t.Fatalf("stddev = %.12f, want %.12f", stats.StdDev(), wantStdDev)
	}
}

func TestSpreadStats_StdDevIsStableForLargeOffsets(t *testing.T) {
	stats := NewStats(8)
	base := 1e12
	window := []float64{
		base + 0,
		base + 1,
		base + 2,
		base + 3,
		base + 4,
		base + 5,
		base + 6,
		base + 7,
	}

	for _, sample := range window {
		stats.Add(sample)
	}

	wantMean, wantStdDev := stableStats(window)
	if math.Abs(stats.Mean()-wantMean) > 1e-6 {
		t.Fatalf("mean = %.6f, want %.6f", stats.Mean(), wantMean)
	}
	if math.Abs(stats.StdDev()-wantStdDev) > 1e-6 {
		t.Fatalf("stddev = %.6f, want %.6f", stats.StdDev(), wantStdDev)
	}
}

func TestSpreadEngine_EmitSignalCallbackCanReenterEngine(t *testing.T) {
	cfg := &appconfig.Config{
		Symbol:               "BTC",
		Quantity:             decimal.RequireFromString("1"),
		LiquidityBufferRatio: 1,
		WindowSize:           10,
		ZOpen:                -1,
		MinProfitBps:         -1,
		WarmupTicks:          1,
		WarmupDuration:       0,
	}

	engine := New(cfg, zap.NewNop().Sugar())
	engine.makerBid = decimal.RequireFromString("100")
	engine.makerAsk = decimal.RequireFromString("101")
	engine.makerBidQty = decimal.RequireFromString("2")
	engine.makerAskQty = decimal.RequireFromString("2")
	engine.takerBid = decimal.RequireFromString("102")
	engine.takerAsk = decimal.RequireFromString("103")
	engine.takerBidQty = decimal.RequireFromString("2")
	engine.takerAskQty = decimal.RequireFromString("2")

	done := make(chan struct{}, 1)
	engine.SetSignalCallback(func(signal *Signal) {
		engine.CurrentZ()
		done <- struct{}{}
	})

	go engine.onBBOUpdate()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("signal callback blocked while reentering engine")
	}
}

func TestSpreadEngine_RoundTripFeeBpsMatchesExecutionPath(t *testing.T) {
	engine := &Engine{}
	engine.SetFees(
		FeeInfo{MakerRate: 0.0001, TakerRate: 0.0003},
		FeeInfo{MakerRate: 0.0002, TakerRate: 0.0005},
	)

	got := engine.RoundTripFeeBps()
	want := (0.0001 + 0.0003 + 0.0005 + 0.0005) * 10000
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("RoundTripFeeBps = %.12f, want %.12f", got, want)
	}
}

func TestSpreadEngine_SignalQuantityUsesSmallerBookSideWithBuffer(t *testing.T) {
	cfg := &appconfig.Config{
		Symbol:               "BTC",
		Quantity:             decimal.RequireFromString("1.00000"),
		LiquidityBufferRatio: 0.8,
		WindowSize:           10,
		ZOpen:                -1,
		MinProfitBps:         -1,
		WarmupTicks:          1,
		WarmupDuration:       0,
	}

	engine := New(cfg, zap.NewNop().Sugar())
	engine.SetFees(FeeInfo{}, FeeInfo{})
	engine.makerBid = decimal.RequireFromString("100")
	engine.makerAsk = decimal.RequireFromString("101")
	engine.makerBidQty = decimal.RequireFromString("3")
	engine.makerAskQty = decimal.RequireFromString("0.6")
	engine.takerBid = decimal.RequireFromString("102")
	engine.takerAsk = decimal.RequireFromString("103")
	engine.takerBidQty = decimal.RequireFromString("0.5")
	engine.takerAskQty = decimal.RequireFromString("4")
	now := time.Now()
	engine.makerTS = now
	engine.takerTS = now

	var got *Signal
	engine.SetSignalCallback(func(signal *Signal) {
		got = signal
	})

	engine.onBBOUpdate()

	if got == nil {
		t.Fatal("expected signal")
	}
	if !got.Quantity.Equal(decimal.RequireFromString("0.4")) {
		t.Fatalf("signal quantity = %s, want 0.4", got.Quantity)
	}
}

func stableStats(samples []float64) (mean float64, stddev float64) {
	if len(samples) == 0 {
		return 0, 0
	}

	for i, sample := range samples {
		delta := sample - mean
		mean += delta / float64(i+1)
		delta2 := sample - mean
		stddev += delta * delta2
	}

	if len(samples) < 2 {
		return mean, 0
	}

	variance := stddev / float64(len(samples)-1) // Bessel's correction for sample variance
	if variance < 0 {
		variance = 0
	}
	return mean, math.Sqrt(variance)
}
