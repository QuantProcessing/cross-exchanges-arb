package main

import (
	"fmt"
	"strings"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
)

// RealizedProfitMetrics summarizes one completed arbitrage round using actual
// entry/exit execution prices plus the configured fee model for the current
// execution path.
type RealizedProfitMetrics struct {
	EntryBps       float64
	ExitBps        float64
	GrossBps       float64
	FeeBps         float64
	NetBps         float64
	GrossQuotePnL  decimal.Decimal
	FeeQuote       decimal.Decimal
	NetQuotePnL    decimal.Decimal
	ReferencePrice decimal.Decimal
}

func calculateRealizedProfitMetrics(
	pos *ArbPosition,
	cfg *Config,
	makerFee, takerFee FeeInfo,
	closeLongOrder, closeShortOrder *exchanges.Order,
) (*RealizedProfitMetrics, error) {
	if pos == nil {
		return nil, fmt.Errorf("nil position")
	}
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}

	longOpenPrice, err := executionPrice(pos.LongOrder)
	if err != nil {
		return nil, fmt.Errorf("long open: %w", err)
	}
	shortOpenPrice, err := executionPrice(pos.ShortOrder)
	if err != nil {
		return nil, fmt.Errorf("short open: %w", err)
	}
	longClosePrice, err := executionPrice(closeLongOrder)
	if err != nil {
		return nil, fmt.Errorf("long close: %w", err)
	}
	shortClosePrice, err := executionPrice(closeShortOrder)
	if err != nil {
		return nil, fmt.Errorf("short close: %w", err)
	}

	entryMid := longOpenPrice.Add(shortOpenPrice).Div(decimal.NewFromInt(2))
	exitMid := longClosePrice.Add(shortClosePrice).Div(decimal.NewFromInt(2))
	refPrice := entryMid.Add(exitMid).Div(decimal.NewFromInt(2))
	if refPrice.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("invalid reference price %s", refPrice)
	}

	entryQuoteEdge := shortOpenPrice.Sub(longOpenPrice)
	exitQuoteEdge := longClosePrice.Sub(shortClosePrice)
	grossQuotePnL := entryQuoteEdge.Add(exitQuoteEdge)

	longOpenFeeRate, err := legFeeRate(pos.LongExchange, cfg, makerFee, takerFee, false)
	if err != nil {
		return nil, fmt.Errorf("long open fee: %w", err)
	}
	shortOpenFeeRate, err := legFeeRate(pos.ShortExchange, cfg, makerFee, takerFee, false)
	if err != nil {
		return nil, fmt.Errorf("short open fee: %w", err)
	}
	longCloseFeeRate, err := legFeeRate(pos.LongExchange, cfg, makerFee, takerFee, true)
	if err != nil {
		return nil, fmt.Errorf("long close fee: %w", err)
	}
	shortCloseFeeRate, err := legFeeRate(pos.ShortExchange, cfg, makerFee, takerFee, true)
	if err != nil {
		return nil, fmt.Errorf("short close fee: %w", err)
	}

	feeQuote := longOpenPrice.Mul(decimal.NewFromFloat(longOpenFeeRate)).
		Add(shortOpenPrice.Mul(decimal.NewFromFloat(shortOpenFeeRate))).
		Add(longClosePrice.Mul(decimal.NewFromFloat(longCloseFeeRate))).
		Add(shortClosePrice.Mul(decimal.NewFromFloat(shortCloseFeeRate)))

	return &RealizedProfitMetrics{
		EntryBps:       bpsFromQuote(entryQuoteEdge, entryMid),
		ExitBps:        bpsFromQuote(exitQuoteEdge, exitMid),
		GrossBps:       bpsFromQuote(grossQuotePnL, refPrice),
		FeeBps:         bpsFromQuote(feeQuote, refPrice),
		NetBps:         bpsFromQuote(grossQuotePnL.Sub(feeQuote), refPrice),
		GrossQuotePnL:  grossQuotePnL,
		FeeQuote:       feeQuote,
		NetQuotePnL:    grossQuotePnL.Sub(feeQuote),
		ReferencePrice: refPrice,
	}, nil
}

func executionPrice(order *exchanges.Order) (decimal.Decimal, error) {
	if order == nil {
		return decimal.Zero, fmt.Errorf("missing order")
	}
	if order.Price.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("missing execution price for order %s", order.OrderID)
	}
	return order.Price, nil
}

func bpsFromQuote(quotePnL, referencePrice decimal.Decimal) float64 {
	if referencePrice.LessThanOrEqual(decimal.Zero) {
		return 0
	}
	bps, _ := quotePnL.Div(referencePrice).Mul(decimal.NewFromInt(10000)).Float64()
	return bps
}

func legFeeRate(exchangeName string, cfg *Config, makerFee, takerFee FeeInfo, isClose bool) (float64, error) {
	switch {
	case strings.EqualFold(exchangeName, cfg.MakerExchange):
		if isClose {
			return makerFee.TakerRate, nil
		}
		return makerFee.MakerRate, nil
	case strings.EqualFold(exchangeName, cfg.TakerExchange):
		return takerFee.TakerRate, nil
	default:
		return 0, fmt.Errorf("unknown exchange %q", exchangeName)
	}
}
