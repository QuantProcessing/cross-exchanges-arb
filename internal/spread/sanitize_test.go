package spread

import (
	"testing"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

func TestSanitizeTopOfBook_ReordersCrossedQuotes(t *testing.T) {
	book := TopOfBook{
		BidPrice: decimal.RequireFromString("68162.9"),
		BidQty:   decimal.RequireFromString("1.2"),
		AskPrice: decimal.RequireFromString("67886"),
		AskQty:   decimal.RequireFromString("0.0001"),
	}

	sanitized := SanitizeTopOfBook(book)
	if !sanitized.WasCrossed {
		t.Fatal("expected crossed book to be flagged")
	}
	if !sanitized.BidPrice.Equal(decimal.RequireFromString("67886")) {
		t.Fatalf("sanitized bid = %s, want 67886", sanitized.BidPrice)
	}
	if !sanitized.AskPrice.Equal(decimal.RequireFromString("68162.9")) {
		t.Fatalf("sanitized ask = %s, want 68162.9", sanitized.AskPrice)
	}
	if !sanitized.BidQty.Equal(decimal.RequireFromString("0.0001")) {
		t.Fatalf("sanitized bid qty = %s, want 0.0001", sanitized.BidQty)
	}
	if !sanitized.AskQty.Equal(decimal.RequireFromString("1.2")) {
		t.Fatalf("sanitized ask qty = %s, want 1.2", sanitized.AskQty)
	}
}

func TestSpreadEngine_DoesNotEmitFakeLongTakerSignalFromCrossedTakerBook(t *testing.T) {
	cfg := &appconfig.Config{
		Symbol:               "BTC",
		Quantity:             decimal.RequireFromString("0.01"),
		LiquidityBufferRatio: 0.8,
		WindowSize:           10,
		ZOpen:                -1,
		MinProfitBps:         20,
		WarmupTicks:          1,
		WarmupDuration:       0,
	}

	engine := New(cfg, zap.NewNop().Sugar())
	engine.SetFees(FeeInfo{}, FeeInfo{})
	engine.makerBid = decimal.RequireFromString("68169.1")
	engine.makerAsk = decimal.RequireFromString("68170.7")
	engine.makerBidQty = decimal.RequireFromString("1")
	engine.makerAskQty = decimal.RequireFromString("1")
	engine.takerBid = decimal.RequireFromString("68162.9")
	engine.takerAsk = decimal.RequireFromString("67886")
	engine.takerBidQty = decimal.RequireFromString("1")
	engine.takerAskQty = decimal.RequireFromString("0.0001")
	now := time.Now()
	engine.makerTS = now
	engine.takerTS = now

	signals := make([]*Signal, 0, 1)
	engine.SetSignalCallback(func(signal *Signal) {
		signals = append(signals, signal)
	})

	engine.onBBOUpdate()

	if len(signals) != 0 {
		t.Fatalf("signals = %d, want 0 because crossed taker ask should be sanitized away", len(signals))
	}

	snapshot := engine.Snapshot()
	if snapshot == nil {
		t.Fatal("snapshot = nil")
	}
	if snapshot.SpreadBA >= 20 {
		t.Fatalf("sanitized BA spread = %.2f, want fake crossed-book edge to collapse below 20bps", snapshot.SpreadBA)
	}
}

func TestSpreadEngine_PreservesLongTakerSignalWhenBookIsExecutable(t *testing.T) {
	cfg := &appconfig.Config{
		Symbol:               "BTC",
		Quantity:             decimal.RequireFromString("0.01"),
		LiquidityBufferRatio: 0.8,
		WindowSize:           10,
		ZOpen:                -1,
		MinProfitBps:         1,
		WarmupTicks:          1,
		WarmupDuration:       0,
	}

	engine := New(cfg, zap.NewNop().Sugar())
	engine.SetFees(FeeInfo{}, FeeInfo{})
	engine.makerBid = decimal.RequireFromString("101")
	engine.makerAsk = decimal.RequireFromString("101.1")
	engine.makerBidQty = decimal.RequireFromString("1")
	engine.makerAskQty = decimal.RequireFromString("1")
	engine.takerBid = decimal.RequireFromString("100")
	engine.takerAsk = decimal.RequireFromString("100.1")
	engine.takerBidQty = decimal.RequireFromString("1")
	engine.takerAskQty = decimal.RequireFromString("1")
	now := time.Now()
	engine.makerTS = now
	engine.takerTS = now

	var got *Signal
	engine.SetSignalCallback(func(signal *Signal) {
		got = signal
	})

	engine.onBBOUpdate()

	if got == nil {
		t.Fatal("expected executable BA signal")
	}
	if got.Direction != LongTakerShortMaker {
		t.Fatalf("direction = %s, want LONG_TAKER_SHORT_MAKER", got.Direction)
	}
	if got.NetEdgeBps <= 0 {
		t.Fatalf("net edge = %.2f, want positive", got.NetEdgeBps)
	}
}
