package app

import (
	"context"
	"errors"
	"testing"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type testExchange struct {
	watchOrderBookErr error
}

func (e *testExchange) GetExchange() string                 { return "TEST" }
func (e *testExchange) GetMarketType() exchanges.MarketType { return exchanges.MarketTypePerp }
func (e *testExchange) Close() error                        { return nil }
func (e *testExchange) FormatSymbol(symbol string) string   { return symbol }
func (e *testExchange) ExtractSymbol(symbol string) string  { return symbol }
func (e *testExchange) ListSymbols() []string               { return []string{"BTC"} }
func (e *testExchange) FetchTicker(ctx context.Context, symbol string) (*exchanges.Ticker, error) {
	return &exchanges.Ticker{LastPrice: decimal.RequireFromString("100")}, nil
}
func (e *testExchange) FetchOrderBook(ctx context.Context, symbol string, limit int) (*exchanges.OrderBook, error) {
	return nil, nil
}
func (e *testExchange) FetchTrades(ctx context.Context, symbol string, limit int) ([]exchanges.Trade, error) {
	return nil, nil
}
func (e *testExchange) FetchKlines(ctx context.Context, symbol string, interval exchanges.Interval, opts *exchanges.KlineOpts) ([]exchanges.Kline, error) {
	return nil, nil
}
func (e *testExchange) PlaceOrder(ctx context.Context, params *exchanges.OrderParams) (*exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) CancelOrder(ctx context.Context, orderID, symbol string) error { return nil }
func (e *testExchange) CancelAllOrders(ctx context.Context, symbol string) error      { return nil }
func (e *testExchange) FetchOrderByID(ctx context.Context, orderID, symbol string) (*exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) FetchOrders(ctx context.Context, symbol string) ([]exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) FetchOpenOrders(ctx context.Context, symbol string) ([]exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) FetchAccount(ctx context.Context) (*exchanges.Account, error) { return nil, nil }
func (e *testExchange) FetchBalance(ctx context.Context) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (e *testExchange) FetchSymbolDetails(ctx context.Context, symbol string) (*exchanges.SymbolDetails, error) {
	return &exchanges.SymbolDetails{
		Symbol:            symbol,
		PricePrecision:    2,
		QuantityPrecision: 3,
		MinQuantity:       decimal.RequireFromString("0.001"),
	}, nil
}
func (e *testExchange) FetchFeeRate(ctx context.Context, symbol string) (*exchanges.FeeRate, error) {
	return &exchanges.FeeRate{
		Maker: decimal.RequireFromString("0.0002"),
		Taker: decimal.RequireFromString("0.0005"),
	}, nil
}
func (e *testExchange) WatchOrderBook(ctx context.Context, symbol string, cb exchanges.OrderBookCallback) error {
	return e.watchOrderBookErr
}
func (e *testExchange) GetLocalOrderBook(symbol string, depth int) *exchanges.OrderBook { return nil }
func (e *testExchange) StopWatchOrderBook(ctx context.Context, symbol string) error     { return nil }
func (e *testExchange) WatchOrders(ctx context.Context, cb exchanges.OrderUpdateCallback) error {
	<-ctx.Done()
	return nil
}
func (e *testExchange) WatchPositions(ctx context.Context, cb exchanges.PositionUpdateCallback) error {
	return nil
}
func (e *testExchange) WatchTicker(ctx context.Context, symbol string, cb exchanges.TickerCallback) error {
	return nil
}
func (e *testExchange) WatchTrades(ctx context.Context, symbol string, cb exchanges.TradeCallback) error {
	return nil
}
func (e *testExchange) WatchKlines(ctx context.Context, symbol string, interval exchanges.Interval, cb exchanges.KlineCallback) error {
	return nil
}
func (e *testExchange) StopWatchOrders(ctx context.Context) error                { return nil }
func (e *testExchange) StopWatchPositions(ctx context.Context) error             { return nil }
func (e *testExchange) StopWatchTicker(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) StopWatchTrades(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) StopWatchKlines(ctx context.Context, symbol string, interval exchanges.Interval) error {
	return nil
}

func TestRun_ObserveOnlyLogsSpreadEngineErrorsAndReturnsNil(t *testing.T) {
	oldNewExchangePair := newExchangePair
	t.Cleanup(func() { newExchangePair = oldNewExchangePair })

	maker := &testExchange{watchOrderBookErr: errors.New("maker stream failed")}
	taker := &testExchange{}
	newExchangePair = func(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
		return maker, taker, nil
	}

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()

	cfg := &appconfig.Config{
		MakerExchange:  "TEST",
		TakerExchange:  "TEST",
		Symbol:         "BTC",
		Quantity:       decimal.RequireFromString("0.001"),
		WindowSize:     10,
		WarmupTicks:    1,
		WarmupDuration: time.Second,
		ObserveOnly:    true,
	}

	err := Run(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if logs.FilterMessage("spread engine error").Len() != 1 {
		t.Fatalf("spread engine error log count = %d, want 1", logs.FilterMessage("spread engine error").Len())
	}
}

func TestRun_ReturnsExchangeCreationErrors(t *testing.T) {
	oldNewExchangePair := newExchangePair
	t.Cleanup(func() { newExchangePair = oldNewExchangePair })

	newExchangePair = func(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
		return nil, nil, errors.New("boom")
	}

	core, _ := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()

	cfg := &appconfig.Config{
		MakerExchange: "TEST",
		TakerExchange: "TEST",
		Symbol:        "BTC",
		Quantity:      decimal.RequireFromString("0.001"),
	}

	err := Run(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
}
