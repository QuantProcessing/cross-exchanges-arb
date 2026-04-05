package trading

import (
	"fmt"
	"strings"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
)

type tradeBBO struct {
	CapturedAt time.Time
	MakerBid   decimal.Decimal
	MakerAsk   decimal.Decimal
	TakerBid   decimal.Decimal
	TakerAsk   decimal.Decimal
	SpreadBps  float64
	ZScore     float64
	Mean       float64
	StdDev     float64
}

type tradeRecapTimings struct {
	SignalToMaker time.Duration
	MakerToFill   time.Duration
	FillToHedge   time.Duration
	Total         time.Duration
}

type openTradeRecap struct {
	Direction            spread.Direction
	Quantity             decimal.Decimal
	MakerExchange        string
	TakerExchange        string
	MakerSide            exchanges.OrderSide
	TakerSide            exchanges.OrderSide
	MakerOrderID         string
	TakerOrderID         string
	MakerOrderPrice      decimal.Decimal
	TakerOrderPrice      decimal.Decimal
	SignalSpreadBps      float64
	SignalZScore         float64
	SignalExpectedProfit float64
	Signal               *tradeBBO
	MakerPlaced          *tradeBBO
	FillDetected         *tradeBBO
	HedgeDone            *tradeBBO
	Timings              tradeRecapTimings
}

func (t *Trader) buildOpenTradeRecap(flow *openFlowState, filledQty decimal.Decimal) *openTradeRecap {
	if flow == nil || flow.signal == nil {
		return nil
	}

	recap := &openTradeRecap{
		Direction:            flow.signal.Direction,
		Quantity:             filledQty,
		MakerExchange:        t.config.MakerExchange,
		TakerExchange:        t.config.TakerExchange,
		MakerSide:            flow.makerSide,
		TakerSide:            flow.takerSide,
		SignalSpreadBps:      flow.signal.SpreadBps,
		SignalZScore:         flow.signal.ZScore,
		SignalExpectedProfit: flow.signal.ExpectedProfit,
		Signal:               cloneTradeBBO(flow.signalSnapshot),
		MakerPlaced:          cloneTradeBBO(flow.makerPlacedSnapshot),
		FillDetected:         cloneTradeBBO(flow.fillSnapshot),
		HedgeDone:            cloneTradeBBO(flow.hedgeSnapshot),
	}
	if flow.makerOrder != nil {
		recap.MakerOrderID = flow.makerOrder.OrderID
		recap.MakerOrderPrice = flow.makerOrder.Price
	}
	if flow.lastHedgeOrder != nil {
		recap.TakerOrderID = flow.lastHedgeOrder.OrderID
		recap.TakerOrderPrice = flow.lastHedgeOrder.Price
	}
	if !flow.signalTime.IsZero() && !flow.makerPlacedAt.IsZero() {
		recap.Timings.SignalToMaker = flow.makerPlacedAt.Sub(flow.signalTime)
	}
	if !flow.makerPlacedAt.IsZero() && !flow.fillDetectedAt.IsZero() {
		recap.Timings.MakerToFill = flow.fillDetectedAt.Sub(flow.makerPlacedAt)
	}
	if !flow.fillDetectedAt.IsZero() && !flow.hedgeDoneAt.IsZero() {
		recap.Timings.FillToHedge = flow.hedgeDoneAt.Sub(flow.fillDetectedAt)
	}
	switch {
	case !flow.signalTime.IsZero() && !flow.hedgeDoneAt.IsZero():
		recap.Timings.Total = flow.hedgeDoneAt.Sub(flow.signalTime)
	case !flow.signalTime.IsZero() && !flow.fillDetectedAt.IsZero():
		recap.Timings.Total = flow.fillDetectedAt.Sub(flow.signalTime)
	case !flow.signalTime.IsZero() && !flow.makerPlacedAt.IsZero():
		recap.Timings.Total = flow.makerPlacedAt.Sub(flow.signalTime)
	}

	return recap
}

func cloneTradeBBO(src *tradeBBO) *tradeBBO {
	if src == nil {
		return nil
	}
	clone := *src
	return &clone
}

func snapshotFromSignal(sig *spread.Signal) *tradeBBO {
	if sig == nil {
		return nil
	}
	capturedAt := sig.Timestamp
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	return &tradeBBO{
		CapturedAt: capturedAt,
		MakerBid:   sig.MakerBid,
		MakerAsk:   sig.MakerAsk,
		TakerBid:   sig.TakerBid,
		TakerAsk:   sig.TakerAsk,
		SpreadBps:  sig.SpreadBps,
		ZScore:     sig.ZScore,
		Mean:       sig.Mean,
		StdDev:     sig.StdDev,
	}
}

func (t *Trader) captureCurrentTradeBBO(direction spread.Direction) *tradeBBO {
	if t == nil || t.engine == nil {
		return nil
	}
	snapshot := t.engine.Snapshot()
	if snapshot == nil {
		return nil
	}

	var (
		spreadBps float64
		mean      float64
		zScore    float64
	)
	zAB, zBA := t.engine.CurrentZ()

	switch direction {
	case spread.LongMakerShortTaker:
		spreadBps = spreadBpsFromQuote(snapshot.MakerAsk, snapshot.TakerBid)
		mean = snapshot.MeanAB
		zScore = zAB
	case spread.LongTakerShortMaker:
		spreadBps = spreadBpsFromQuote(snapshot.TakerAsk, snapshot.MakerBid)
		mean = snapshot.MeanBA
		zScore = zBA
	}

	return &tradeBBO{
		CapturedAt: time.Now(),
		MakerBid:   snapshot.MakerBid,
		MakerAsk:   snapshot.MakerAsk,
		TakerBid:   snapshot.TakerBid,
		TakerAsk:   snapshot.TakerAsk,
		SpreadBps:  spreadBps,
		ZScore:     zScore,
		Mean:       mean,
	}
}

func spreadBpsFromQuote(longPrice, shortPrice decimal.Decimal) float64 {
	if longPrice.IsZero() || shortPrice.IsZero() {
		return 0
	}
	mid := longPrice.Add(shortPrice).Div(decimal.NewFromInt(2))
	if mid.IsZero() {
		return 0
	}
	value, _ := shortPrice.Sub(longPrice).Div(mid).Mul(decimal.NewFromInt(10000)).Float64()
	return value
}

func (r *openTradeRecap) Summary() string {
	if r == nil {
		return ""
	}

	parts := []string{
		fmt.Sprintf("timing[maker=%dms fill=%dms hedge=%dms total=%dms]",
			r.Timings.SignalToMaker.Milliseconds(),
			r.Timings.MakerToFill.Milliseconds(),
			r.Timings.FillToHedge.Milliseconds(),
			r.Timings.Total.Milliseconds(),
		),
		fmt.Sprintf("orders[maker=%s@%s taker=%s@%s]",
			r.MakerOrderID, formatDecimal(r.MakerOrderPrice),
			r.TakerOrderID, formatDecimal(r.TakerOrderPrice),
		),
		fmt.Sprintf("signal_bbo[%s]", formatTradeBBO(r.Signal)),
		fmt.Sprintf("maker_bbo[%s]", formatTradeBBO(r.MakerPlaced)),
		fmt.Sprintf("fill_bbo[%s]", formatTradeBBO(r.FillDetected)),
		fmt.Sprintf("hedge_bbo[%s]", formatTradeBBO(r.HedgeDone)),
	}
	return strings.Join(parts, " ")
}

func formatTradeBBO(snapshot *tradeBBO) string {
	if snapshot == nil {
		return "n/a"
	}

	parts := []string{
		fmt.Sprintf("ts=%s", snapshot.CapturedAt.Format(time.RFC3339Nano)),
		fmt.Sprintf("mbid=%s", formatDecimal(snapshot.MakerBid)),
		fmt.Sprintf("mask=%s", formatDecimal(snapshot.MakerAsk)),
		fmt.Sprintf("tbid=%s", formatDecimal(snapshot.TakerBid)),
		fmt.Sprintf("task=%s", formatDecimal(snapshot.TakerAsk)),
	}
	if snapshot.SpreadBps != 0 {
		parts = append(parts, fmt.Sprintf("spread=%.1fbps", snapshot.SpreadBps))
	}
	if snapshot.ZScore != 0 {
		parts = append(parts, fmt.Sprintf("z=%.2f", snapshot.ZScore))
	}
	if snapshot.Mean != 0 {
		parts = append(parts, fmt.Sprintf("mean=%.2f", snapshot.Mean))
	}
	return strings.Join(parts, " ")
}

func formatDecimal(value decimal.Decimal) string {
	if value.IsZero() {
		return "0"
	}
	return value.String()
}
