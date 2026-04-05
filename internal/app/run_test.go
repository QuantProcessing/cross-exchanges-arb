package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/marketdata"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type testExchange struct {
	watchOrderBookErr   error
	watchOrdersErr      error
	watchPositionsErr   error
	localBook           *exchanges.OrderBook
	emitInitialBook     bool
	fetchAccountCalls   int
	watchOrdersCalls    int
	watchPositionsCalls int
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
func (e *testExchange) PlaceOrderWS(ctx context.Context, params *exchanges.OrderParams) error {
	return nil
}
func (e *testExchange) CancelOrder(ctx context.Context, orderID, symbol string) error   { return nil }
func (e *testExchange) CancelOrderWS(ctx context.Context, orderID, symbol string) error { return nil }
func (e *testExchange) CancelAllOrders(ctx context.Context, symbol string) error        { return nil }
func (e *testExchange) FetchOrderByID(ctx context.Context, orderID, symbol string) (*exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) FetchOrders(ctx context.Context, symbol string) ([]exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) FetchOpenOrders(ctx context.Context, symbol string) ([]exchanges.Order, error) {
	return nil, nil
}
func (e *testExchange) FetchAccount(ctx context.Context) (*exchanges.Account, error) {
	e.fetchAccountCalls++
	return &exchanges.Account{}, nil
}
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
func (e *testExchange) WatchOrderBook(ctx context.Context, symbol string, depth int, cb exchanges.OrderBookCallback) error {
	if e.watchOrderBookErr != nil {
		return e.watchOrderBookErr
	}
	if e.emitInitialBook && cb != nil && e.localBook != nil {
		go func() {
			time.Sleep(10 * time.Millisecond)
			cb(e.localBook)
		}()
	}
	<-ctx.Done()
	return ctx.Err()
}
func (e *testExchange) GetLocalOrderBook(symbol string, depth int) *exchanges.OrderBook {
	if e.localBook == nil {
		return nil
	}
	book := &exchanges.OrderBook{
		Symbol:    e.localBook.Symbol,
		Timestamp: e.localBook.Timestamp,
	}
	if len(e.localBook.Bids) > depth {
		book.Bids = append(book.Bids, e.localBook.Bids[:depth]...)
	} else {
		book.Bids = append(book.Bids, e.localBook.Bids...)
	}
	if len(e.localBook.Asks) > depth {
		book.Asks = append(book.Asks, e.localBook.Asks[:depth]...)
	} else {
		book.Asks = append(book.Asks, e.localBook.Asks...)
	}
	return book
}
func (e *testExchange) StopWatchOrderBook(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) WatchOrders(ctx context.Context, cb exchanges.OrderUpdateCallback) error {
	e.watchOrdersCalls++
	return e.watchOrdersErr
}
func (e *testExchange) WatchFills(ctx context.Context, cb exchanges.FillCallback) error {
	<-ctx.Done()
	return nil
}
func (e *testExchange) WatchPositions(ctx context.Context, cb exchanges.PositionUpdateCallback) error {
	e.watchPositionsCalls++
	return e.watchPositionsErr
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
func (e *testExchange) StopWatchFills(ctx context.Context) error                 { return nil }
func (e *testExchange) StopWatchPositions(ctx context.Context) error             { return nil }
func (e *testExchange) StopWatchTicker(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) StopWatchTrades(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) StopWatchKlines(ctx context.Context, symbol string, interval exchanges.Interval) error {
	return nil
}

func TestRun_LogsSpreadEngineErrorsAndReturnsNil(t *testing.T) {
	oldNewExchangePair := newExchangePair
	t.Cleanup(func() { newExchangePair = oldNewExchangePair })

	maker := &testExchange{watchOrderBookErr: errors.New("maker stream failed")}
	taker := &testExchange{}
	newExchangePair = func(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
		return maker, taker, nil
	}

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	withTempWorkingDir(t)

	cfg := &appconfig.Config{
		MakerExchange:  "TEST",
		TakerExchange:  "TEST",
		Symbol:         "BTC",
		Quantity:       decimal.RequireFromString("0.001"),
		WindowSize:     10,
		WarmupTicks:    1,
		WarmupDuration: time.Second,
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
	withTempWorkingDir(t)

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

func TestRun_StartsTradingAccounts(t *testing.T) {
	oldNewExchangePair := newExchangePair
	t.Cleanup(func() { newExchangePair = oldNewExchangePair })

	book := &exchanges.OrderBook{
		Symbol:    "BTC",
		Timestamp: 1710000000000,
		Bids: []exchanges.Level{
			{Price: decimal.RequireFromString("100"), Quantity: decimal.RequireFromString("1")},
		},
		Asks: []exchanges.Level{
			{Price: decimal.RequireFromString("100.1"), Quantity: decimal.RequireFromString("1")},
		},
	}
	maker := &testExchange{localBook: book, emitInitialBook: true}
	taker := &testExchange{localBook: book, emitInitialBook: true}
	newExchangePair = func(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
		return maker, taker, nil
	}

	core, _ := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	withTempWorkingDir(t)

	cfg := &appconfig.Config{
		MakerExchange:  "TEST",
		TakerExchange:  "TEST",
		Symbol:         "BTC",
		Quantity:       decimal.RequireFromString("0.001"),
		WindowSize:     10,
		WarmupTicks:    1,
		WarmupDuration: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	err := Run(ctx, cfg, logger)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run() error = %v", err)
	}

	if maker.fetchAccountCalls == 0 || taker.fetchAccountCalls == 0 {
		t.Fatalf("FetchAccount calls maker=%d taker=%d, want both > 0", maker.fetchAccountCalls, taker.fetchAccountCalls)
	}
	if maker.watchOrdersCalls == 0 || taker.watchOrdersCalls == 0 {
		t.Fatalf("WatchOrders calls maker=%d taker=%d, want both > 0", maker.watchOrdersCalls, taker.watchOrdersCalls)
	}
	if maker.watchPositionsCalls == 0 || taker.watchPositionsCalls == 0 {
		t.Fatalf("WatchPositions calls maker=%d taker=%d, want both > 0", maker.watchPositionsCalls, taker.watchPositionsCalls)
	}
}

func TestRun_WritesRunArtifactsWhenConfigured(t *testing.T) {
	oldNewExchangePair := newExchangePair
	oldNowFunc := nowFunc
	t.Cleanup(func() { newExchangePair = oldNewExchangePair })
	t.Cleanup(func() { nowFunc = oldNowFunc })

	book := &exchanges.OrderBook{
		Symbol:    "BTC",
		Timestamp: 1710000000000,
		Bids: []exchanges.Level{
			{Price: decimal.RequireFromString("100"), Quantity: decimal.RequireFromString("1")},
			{Price: decimal.RequireFromString("99.9"), Quantity: decimal.RequireFromString("1")},
			{Price: decimal.RequireFromString("99.8"), Quantity: decimal.RequireFromString("1")},
		},
		Asks: []exchanges.Level{
			{Price: decimal.RequireFromString("100.1"), Quantity: decimal.RequireFromString("1")},
			{Price: decimal.RequireFromString("100.2"), Quantity: decimal.RequireFromString("1")},
			{Price: decimal.RequireFromString("100.3"), Quantity: decimal.RequireFromString("1")},
		},
	}
	maker := &testExchange{localBook: book, emitInitialBook: true}
	taker := &testExchange{localBook: book, emitInitialBook: true}
	newExchangePair = func(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
		return maker, taker, nil
	}

	core, _ := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	cwd := withTempWorkingDir(t)
	nowFunc = func() time.Time { return time.Date(2026, 4, 2, 1, 2, 3, 0, time.UTC) }
	runDir := cwd + "/logs/20260402_010203_EDGEX_LIGHTER_BTC"
	rawPath := runDir + "/raw.jsonl"
	runLogPath := runDir + "/run.log"
	cfg := &appconfig.Config{
		MakerExchange:  "EDGEX",
		TakerExchange:  "LIGHTER",
		Symbol:         "BTC",
		Quantity:       decimal.RequireFromString("0.001"),
		WindowSize:     10,
		WarmupTicks:    1,
		WarmupDuration: time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	if err := Run(ctx, cfg, logger); err != nil && err != context.Canceled {
		t.Fatalf("Run() error = %v", err)
	}

	data, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", rawPath, err)
	}
	if len(data) == 0 {
		t.Fatal("raw.jsonl should not be empty")
	}
	logData, err := os.ReadFile(runLogPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", runLogPath, err)
	}
	if !strings.Contains(string(logData), "MKT updated=") {
		t.Fatalf("run.log should contain market detail lines, got: %s", string(logData))
	}
}

func TestMetricsStatsFromSnapshot_RoundsBpsFieldsToOneDecimal(t *testing.T) {
	stats := metricsStatsFromSnapshot(&spread.Snapshot{
		SpreadAB: 12.34,
		SpreadBA: -1.26,
		MeanAB:   2.25,
		MeanBA:   -0.04,
		StdDevAB: 0.15,
		StdDevBA: 1.96,
	}, marketdata.MarketFrame{})

	if stats.SpreadABBps != 12.3 {
		t.Fatalf("SpreadABBps = %v, want 12.3", stats.SpreadABBps)
	}
	if stats.SpreadBABps != -1.3 {
		t.Fatalf("SpreadBABps = %v, want -1.3", stats.SpreadBABps)
	}
	if stats.MeanAB != 2.3 {
		t.Fatalf("MeanAB = %v, want 2.3", stats.MeanAB)
	}
	if stats.MeanBA != 0 {
		t.Fatalf("MeanBA = %v, want 0.0", stats.MeanBA)
	}
	if stats.StdAB != 0.2 {
		t.Fatalf("StdAB = %v, want 0.2", stats.StdAB)
	}
	if stats.StdBA != 2.0 {
		t.Fatalf("StdBA = %v, want 2.0", stats.StdBA)
	}
	if stats.ZAB == 0 {
		t.Fatal("ZAB should still be computed, got 0")
	}
}

func withTempWorkingDir(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir(%s): %v", cwd, err)
	}
	return cwd
}
