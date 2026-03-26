package main

import (
	"testing"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

func newTestTrader() *Trader {
	return &Trader{
		config: &Config{
			Symbol:   "BTC",
			Quantity: decimal.RequireFromString("0.001"),
		},
		logger: zap.NewNop().Sugar(),
		state:  StateIdle,
	}
}

func TestTrader_MakerTimeoutReturnsToIdle(t *testing.T) {
	tr := newTestTrader()
	tr.state = StateWaitingFill

	tr.handleMakerTimeoutForTest()

	if tr.state != StateIdle {
		t.Fatalf("state = %s, want idle", tr.state)
	}
}

func TestTrader_PartialFillTriggersImmediateHedge(t *testing.T) {
	tr := newTestTrader()
	err := tr.handleMakerFillForTest(decimal.RequireFromString("0.001"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.state != StatePositionOpen {
		t.Fatalf("state = %s, want position_open", tr.state)
	}
}
