package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
)

type testExchange struct {
	name        string
	localBook   *exchanges.OrderBook
	watchBookCB exchanges.OrderBookCallback
}

func (e *testExchange) GetExchange() string                 { return e.name }
func (e *testExchange) GetMarketType() exchanges.MarketType { return exchanges.MarketTypePerp }
func (e *testExchange) Close() error                        { return nil }
func (e *testExchange) FormatSymbol(symbol string) string   { return symbol }
func (e *testExchange) ExtractSymbol(symbol string) string  { return symbol }
func (e *testExchange) ListSymbols() []string               { return []string{"BTC"} }
func (e *testExchange) FetchTicker(ctx context.Context, symbol string) (*exchanges.Ticker, error) {
	return nil, nil
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
func (e *testExchange) CancelOrder(ctx context.Context, orderID, symbol string) error { return nil }
func (e *testExchange) CancelOrderWS(ctx context.Context, orderID, symbol string) error {
	return nil
}
func (e *testExchange) CancelAllOrders(ctx context.Context, symbol string) error { return nil }
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
	return &exchanges.Account{}, nil
}
func (e *testExchange) FetchBalance(ctx context.Context) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (e *testExchange) FetchSymbolDetails(ctx context.Context, symbol string) (*exchanges.SymbolDetails, error) {
	return nil, nil
}
func (e *testExchange) FetchFeeRate(ctx context.Context, symbol string) (*exchanges.FeeRate, error) {
	return nil, nil
}
func (e *testExchange) WatchOrderBook(ctx context.Context, symbol string, depth int, cb exchanges.OrderBookCallback) error {
	e.watchBookCB = cb
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
	<-ctx.Done()
	return ctx.Err()
}
func (e *testExchange) WatchFills(ctx context.Context, cb exchanges.FillCallback) error {
	<-ctx.Done()
	return ctx.Err()
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
func (e *testExchange) StopWatchFills(ctx context.Context) error                 { return nil }
func (e *testExchange) StopWatchPositions(ctx context.Context) error             { return nil }
func (e *testExchange) StopWatchTicker(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) StopWatchTrades(ctx context.Context, symbol string) error { return nil }
func (e *testExchange) StopWatchKlines(ctx context.Context, symbol string, interval exchanges.Interval) error {
	return nil
}

func TestRecorderWritesOneJsonLinePerFrame(t *testing.T) {
	var buf bytes.Buffer
	recorder := NewRecorder("EDGEX", "LIGHTER", "BTC", &buf)

	frame := MarketFrame{
		UpdatedSide:     SideMaker,
		Symbol:          "BTC",
		MakerExchange:   "EDGEX",
		TakerExchange:   "LIGHTER",
		LocalTime:       time.Unix(1710000000, 0).UTC(),
		MakerExchangeTS: time.Unix(1710000000, 0).UTC(),
		TakerExchangeTS: time.Unix(1710000001, 0).UTC(),
		MakerBook: MarketBook{
			Bids: []MarketLevel{
				{Price: decimal.RequireFromString("100"), Quantity: decimal.RequireFromString("1")},
				{Price: decimal.RequireFromString("99.9"), Quantity: decimal.RequireFromString("2")},
			},
			Asks: []MarketLevel{
				{Price: decimal.RequireFromString("101"), Quantity: decimal.RequireFromString("1")},
				{Price: decimal.RequireFromString("101.1"), Quantity: decimal.RequireFromString("2")},
			},
		},
		TakerBook: MarketBook{
			Bids: []MarketLevel{
				{Price: decimal.RequireFromString("102"), Quantity: decimal.RequireFromString("1")},
				{Price: decimal.RequireFromString("101.9"), Quantity: decimal.RequireFromString("2")},
			},
			Asks: []MarketLevel{
				{Price: decimal.RequireFromString("103"), Quantity: decimal.RequireFromString("1")},
				{Price: decimal.RequireFromString("103.1"), Quantity: decimal.RequireFromString("2")},
			},
		},
	}

	if err := recorder.Record(frame); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("jsonl lines = %d, want 2", len(lines))
	}
	if bytes.Contains(lines[0], []byte("schema_version")) || bytes.Contains(lines[1], []byte("schema_version")) {
		t.Fatal("raw records must not contain schema_version")
	}

	var maker RawRecord
	if err := json.Unmarshal(lines[0], &maker); err != nil {
		t.Fatalf("unmarshal maker jsonl line: %v", err)
	}
	if maker.Symbol != "BTC" {
		t.Fatalf("decoded symbol = %q, want BTC", maker.Symbol)
	}
	if maker.Side != SideMaker {
		t.Fatalf("maker side = %s, want maker", maker.Side)
	}
	if maker.Exchange != "EDGEX" {
		t.Fatalf("maker exchange = %q, want EDGEX", maker.Exchange)
	}
	if len(maker.Bids) != 2 || len(maker.Asks) != 2 {
		t.Fatalf("maker depth = bids:%d asks:%d, want 2/2", len(maker.Bids), len(maker.Asks))
	}

	var taker RawRecord
	if err := json.Unmarshal(lines[1], &taker); err != nil {
		t.Fatalf("unmarshal taker jsonl line: %v", err)
	}
	if taker.Side != SideTaker {
		t.Fatalf("taker side = %s, want taker", taker.Side)
	}
	if taker.Exchange != "LIGHTER" {
		t.Fatalf("taker exchange = %q, want LIGHTER", taker.Exchange)
	}
}

func TestServiceEmitsUnifiedTop2FrameOnUpdate(t *testing.T) {
	maker := &testExchange{name: "EDGEX", localBook: testOrderBook(1000)}
	taker := &testExchange{name: "LIGHTER", localBook: testOrderBook(2000)}
	service := NewService(maker, taker, "BTC", "EDGEX", "LIGHTER")

	frames := make([]MarketFrame, 0, 1)
	service.Subscribe(func(frame MarketFrame) {
		frames = append(frames, frame)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go service.Start(ctx)

	waitForCallback(t, 200*time.Millisecond, func() bool { return maker.watchBookCB != nil && taker.watchBookCB != nil })

	maker.watchBookCB(maker.localBook)

	waitForCallback(t, 200*time.Millisecond, func() bool { return len(frames) == 1 })

	frame := frames[0]
	if frame.UpdatedSide != SideMaker {
		t.Fatalf("updated side = %s, want maker", frame.UpdatedSide)
	}
	if len(frame.MakerBook.Bids) != 2 || len(frame.TakerBook.Asks) != 2 {
		t.Fatalf("frame top2 depth not preserved: maker bids=%d taker asks=%d", len(frame.MakerBook.Bids), len(frame.TakerBook.Asks))
	}
}

func testOrderBook(base int64) *exchanges.OrderBook {
	return &exchanges.OrderBook{
		Symbol:    "BTC",
		Timestamp: base,
		Bids: []exchanges.Level{
			{Price: decimal.NewFromInt(base + 1), Quantity: decimal.RequireFromString("1.0")},
			{Price: decimal.NewFromInt(base + 0), Quantity: decimal.RequireFromString("2.0")},
			{Price: decimal.NewFromInt(base - 1), Quantity: decimal.RequireFromString("3.0")},
		},
		Asks: []exchanges.Level{
			{Price: decimal.NewFromInt(base + 2), Quantity: decimal.RequireFromString("1.5")},
			{Price: decimal.NewFromInt(base + 3), Quantity: decimal.RequireFromString("2.5")},
			{Price: decimal.NewFromInt(base + 4), Quantity: decimal.RequireFromString("3.5")},
		},
	}
}

func waitForCallback(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
