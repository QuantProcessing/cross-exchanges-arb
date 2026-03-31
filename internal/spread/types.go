package spread

import (
	"time"

	"github.com/shopspring/decimal"
)

// Direction indicates which direction to trade.
type Direction string

const (
	// LongMakerShortTaker: buy on maker exchange, sell on taker exchange.
	LongMakerShortTaker Direction = "LONG_MAKER_SHORT_TAKER"
	// LongTakerShortMaker: buy on taker exchange, sell on maker exchange.
	LongTakerShortMaker Direction = "LONG_TAKER_SHORT_MAKER"
)

// Signal represents a detected arbitrage opportunity.
type Signal struct {
	Direction      Direction
	SpreadBps      float64
	ZScore         float64
	Mean           float64
	StdDev         float64
	ExpectedProfit float64
	MakerBid       decimal.Decimal
	MakerAsk       decimal.Decimal
	TakerBid       decimal.Decimal
	TakerAsk       decimal.Decimal
	Timestamp      time.Time
}

// Snapshot captures the current top-of-book and rolling spread statistics.
type Snapshot struct {
	MakerBid decimal.Decimal
	MakerAsk decimal.Decimal
	TakerBid decimal.Decimal
	TakerAsk decimal.Decimal
	MeanAB   float64
	MeanBA   float64
}

// FeeInfo holds maker/taker fee rates for an exchange.
type FeeInfo struct {
	MakerRate float64
	TakerRate float64
}
