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
	Direction           Direction
	SpreadBps           float64
	ZScore              float64
	Mean                float64
	StdDev              float64
	ExpectedProfit      float64
	NetEdgeBps          float64
	DynamicThresholdBps float64
	Quantity            decimal.Decimal
	MakerBid            decimal.Decimal
	MakerAsk            decimal.Decimal
	TakerBid            decimal.Decimal
	TakerAsk            decimal.Decimal
	MakerBidQty         decimal.Decimal
	MakerAskQty         decimal.Decimal
	TakerBidQty         decimal.Decimal
	TakerAskQty         decimal.Decimal
	Timestamp           time.Time
}

// Snapshot captures the current top-of-book and rolling spread statistics.
type Snapshot struct {
	MakerBid    decimal.Decimal
	MakerAsk    decimal.Decimal
	TakerBid    decimal.Decimal
	TakerAsk    decimal.Decimal
	MakerBidQty decimal.Decimal
	MakerAskQty decimal.Decimal
	TakerBidQty decimal.Decimal
	TakerAskQty decimal.Decimal
	MakerTS     time.Time
	TakerTS     time.Time
	SpreadAB    float64
	SpreadBA    float64
	MeanAB      float64
	MeanBA      float64
	StdDevAB    float64
	StdDevBA    float64
	ValidAB     bool
	ValidBA     bool
	ReasonAB    string
	ReasonBA    string
}

// FeeInfo holds maker/taker fee rates for an exchange.
type FeeInfo struct {
	MakerRate float64
	TakerRate float64
}

// TopOfBook captures the best bid/ask and size for a market snapshot.
type TopOfBook struct {
	BidPrice  decimal.Decimal
	BidQty    decimal.Decimal
	AskPrice  decimal.Decimal
	AskQty    decimal.Decimal
	Timestamp time.Time
}

// SanitizedTopOfBook normalizes a top-of-book into non-crossed bid/ask order.
type SanitizedTopOfBook struct {
	BidPrice   decimal.Decimal
	BidQty     decimal.Decimal
	AskPrice   decimal.Decimal
	AskQty     decimal.Decimal
	Timestamp  time.Time
	WasCrossed bool
}
