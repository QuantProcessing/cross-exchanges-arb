package trading

import (
	"testing"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	"github.com/shopspring/decimal"
)

func TestTrader_CloseUsesExecutableSpreadThreshold(t *testing.T) {
	tr := newTestTraderWithPosition()
	tr.config.ZClose = 0.5

	engine := &testEngine{
		currentZAB: 9.0,
	}
	engine.setSnapshot(spread.Snapshot{
		MakerBid:    decimal.RequireFromString("100"),
		MakerAsk:    decimal.RequireFromString("100.2"),
		TakerBid:    decimal.RequireFromString("100.1"),
		TakerAsk:    decimal.RequireFromString("100.3"),
		SpreadAB:    1.0,
		MeanAB:      2.0,
		StdDevAB:    0.5,
		ValidAB:     true,
		MakerTS:     time.Now(),
		TakerTS:     time.Now(),
		MakerBidQty: decimal.RequireFromString("1"),
		MakerAskQty: decimal.RequireFromString("1"),
		TakerBidQty: decimal.RequireFromString("1"),
		TakerAskQty: decimal.RequireFromString("1"),
	})
	tr.engine = engine

	tr.checkCloseConditions()

	if tr.state != StateCooldown {
		t.Fatalf("state = %s, want cooldown because executable spread reverted below threshold", tr.state)
	}
}
