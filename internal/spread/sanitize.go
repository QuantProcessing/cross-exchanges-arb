package spread

import (
	"fmt"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/shopspring/decimal"
)

// SanitizeTopOfBook reorders crossed bid/ask pairs so downstream logic always
// sees a non-inverted top of book. This preserves both strategy directions while
// filtering obvious bad top-level quotes out of spread calculations.
func SanitizeTopOfBook(book TopOfBook) SanitizedTopOfBook {
	sanitized := SanitizedTopOfBook{
		BidPrice:  book.BidPrice,
		BidQty:    book.BidQty,
		AskPrice:  book.AskPrice,
		AskQty:    book.AskQty,
		Timestamp: book.Timestamp,
	}

	if !book.AskPrice.IsZero() && !book.BidPrice.IsZero() && book.AskPrice.LessThan(book.BidPrice) {
		sanitized.WasCrossed = true
		sanitized.BidPrice = book.AskPrice
		sanitized.BidQty = book.AskQty
		sanitized.AskPrice = book.BidPrice
		sanitized.AskQty = book.BidQty
	}

	return sanitized
}

func sanitizeSnapshot(
	cfg *appconfig.Config,
	maker TopOfBook,
	taker TopOfBook,
	meanAB, meanBA, stddevAB, stddevBA float64,
	now time.Time,
) Snapshot {
	makerBook := SanitizeTopOfBook(maker)
	takerBook := SanitizeTopOfBook(taker)

	snapshot := Snapshot{
		MakerBid:    makerBook.BidPrice,
		MakerAsk:    makerBook.AskPrice,
		TakerBid:    takerBook.BidPrice,
		TakerAsk:    takerBook.AskPrice,
		MakerBidQty: makerBook.BidQty,
		MakerAskQty: makerBook.AskQty,
		TakerBidQty: takerBook.BidQty,
		TakerAskQty: takerBook.AskQty,
		MakerTS:     makerBook.Timestamp,
		TakerTS:     takerBook.Timestamp,
		MeanAB:      meanAB,
		MeanBA:      meanBA,
		StdDevAB:    stddevAB,
		StdDevBA:    stddevBA,
	}

	snapshot.SpreadAB = spreadBps(snapshot.MakerAsk, snapshot.TakerBid)
	snapshot.SpreadBA = spreadBps(snapshot.TakerAsk, snapshot.MakerBid)
	snapshot.ValidAB, snapshot.ReasonAB = snapshot.directionExecutable(cfg, LongMakerShortTaker, now)
	snapshot.ValidBA, snapshot.ReasonBA = snapshot.directionExecutable(cfg, LongTakerShortMaker, now)

	return snapshot
}

func (s Snapshot) directionExecutable(cfg *appconfig.Config, direction Direction, now time.Time) (bool, string) {
	if cfg == nil {
		return false, "config missing"
	}

	switch direction {
	case LongMakerShortTaker:
		if s.MakerAsk.IsZero() || s.TakerBid.IsZero() {
			return false, "missing maker ask or taker bid"
		}
		if !quoteFresh(s.MakerTS, cfg.MaxQuoteAge, now) || !quoteFresh(s.TakerTS, cfg.MaxQuoteAge, now) {
			return false, "stale quote"
		}
		if s.executableQuantity(cfg, direction).LessThanOrEqual(decimal.Zero) {
			return false, "insufficient top-level quantity"
		}
		return true, ""
	case LongTakerShortMaker:
		if s.TakerAsk.IsZero() || s.MakerBid.IsZero() {
			return false, "missing taker ask or maker bid"
		}
		if !quoteFresh(s.MakerTS, cfg.MaxQuoteAge, now) || !quoteFresh(s.TakerTS, cfg.MaxQuoteAge, now) {
			return false, "stale quote"
		}
		if s.executableQuantity(cfg, direction).LessThanOrEqual(decimal.Zero) {
			return false, "insufficient top-level quantity"
		}
		return true, ""
	default:
		return false, fmt.Sprintf("unsupported direction %s", direction)
	}
}

func (s Snapshot) executableQuantity(cfg *appconfig.Config, direction Direction) decimal.Decimal {
	if cfg == nil {
		return decimal.Zero
	}

	limit := cfg.Quantity
	if limit.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}

	buffer := decimal.NewFromFloat(cfg.LiquidityBufferRatio)
	if buffer.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}

	var thinner decimal.Decimal
	switch direction {
	case LongMakerShortTaker:
		thinner = decimal.Min(s.MakerAskQty, s.TakerBidQty)
	case LongTakerShortMaker:
		thinner = decimal.Min(s.TakerAskQty, s.MakerBidQty)
	default:
		return decimal.Zero
	}

	if thinner.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}

	qty := thinner.Mul(buffer)
	if qty.GreaterThan(limit) {
		qty = limit
	}
	if qty.IsNegative() {
		return decimal.Zero
	}
	return qty
}

func quoteFresh(ts time.Time, maxAge time.Duration, now time.Time) bool {
	if maxAge <= 0 || ts.IsZero() {
		return true
	}
	return now.Sub(ts) <= maxAge
}

func spreadBps(longPrice, shortPrice decimal.Decimal) float64 {
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
