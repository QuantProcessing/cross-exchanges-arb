package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

type testExchange struct {
	mu              sync.Mutex
	name            string
	symbolDetails   *exchanges.SymbolDetails
	placedQtys      []decimal.Decimal
	cancelCalls     int
	lastCancelOrder string
}

func newTestExchange(name string) *testExchange {
	return &testExchange{
		name: name,
		symbolDetails: &exchanges.SymbolDetails{
			Symbol:            "BTC",
			PricePrecision:    2,
			QuantityPrecision: 3,
		},
	}
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
	e.mu.Lock()
	defer e.mu.Unlock()
	e.placedQtys = append(e.placedQtys, params.Quantity)
	return &exchanges.Order{
		OrderID:        fmt.Sprintf("%s-%d", e.name, len(e.placedQtys)),
		Symbol:         params.Symbol,
		Side:           params.Side,
		Type:           params.Type,
		Quantity:       params.Quantity,
		Status:         exchanges.OrderStatusFilled,
		FilledQuantity: params.Quantity,
		ClientOrderID:  params.ClientID,
	}, nil
}
func (e *testExchange) CancelOrder(ctx context.Context, orderID, symbol string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelCalls++
	e.lastCancelOrder = orderID
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
func (e *testExchange) FetchAccount(ctx context.Context) (*exchanges.Account, error) { return nil, nil }
func (e *testExchange) FetchBalance(ctx context.Context) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (e *testExchange) FetchSymbolDetails(ctx context.Context, symbol string) (*exchanges.SymbolDetails, error) {
	return e.symbolDetails, nil
}
func (e *testExchange) FetchFeeRate(ctx context.Context, symbol string) (*exchanges.FeeRate, error) {
	return nil, nil
}
func (e *testExchange) WatchOrderBook(ctx context.Context, symbol string, cb exchanges.OrderBookCallback) error {
	return nil
}
func (e *testExchange) GetLocalOrderBook(symbol string, depth int) *exchanges.OrderBook { return nil }
func (e *testExchange) StopWatchOrderBook(ctx context.Context, symbol string) error     { return nil }
func (e *testExchange) WatchOrders(ctx context.Context, cb exchanges.OrderUpdateCallback) error {
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

func newActiveFlowTrader() (*Trader, *testExchange, *testExchange) {
	maker := newTestExchange("maker")
	taker := newTestExchange("taker")
	qty := decimal.RequireFromString("0.003")

	tr := &Trader{
		maker: maker,
		taker: taker,
		config: &Config{
			MakerExchange: "maker",
			TakerExchange: "taker",
			Symbol:        "BTC",
			Quantity:      qty,
			Slippage:      0.001,
			MakerTimeout:  1 * time.Second,
		},
		logger:       zap.NewNop().Sugar(),
		makerOrderCh: make(chan *exchanges.Order, 10),
		takerOrderCh: make(chan *exchanges.Order, 10),
		state:        StateIdle,
	}

	tr.openFlow = &openFlowState{
		signal: &SpreadSignal{
			Direction:      LongMakerShortTaker,
			SpreadBps:      12.5,
			ZScore:         2.1,
			ExpectedProfit: 4.2,
		},
		makerOrder: &exchanges.Order{
			OrderID:       "maker-1",
			ClientOrderID: "cid-1",
		},
		takerSide: exchanges.OrderSideSell,
		makerQty:  qty,
	}

	return tr, maker, taker
}

func waitForTraderState(t *testing.T, tr *Trader, want ExecutionState) {
	t.Helper()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		state := tr.state
		tr.mu.Unlock()
		if state == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	tr.mu.Lock()
	got := tr.state
	tr.mu.Unlock()
	t.Fatalf("state = %s, want %s", got, want)
}

func waitForMakerOrders(t *testing.T, maker *testExchange, want int) {
	t.Helper()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		maker.mu.Lock()
		got := len(maker.placedQtys)
		maker.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	maker.mu.Lock()
	got := len(maker.placedQtys)
	maker.mu.Unlock()
	t.Fatalf("maker order count = %d, want %d", got, want)
}

func TestTrader_MakerTimeoutKeepsBlockedWhenSettlementUnknown(t *testing.T) {
	tr, _, _ := newActiveFlowTrader()

	tr.handleMakerTimeoutForTest()

	if tr.state == StateIdle {
		t.Fatalf("state = %s, want blocked open-flow state", tr.state)
	}
	if tr.openFlow == nil {
		t.Fatal("openFlow was cleared, want active open-flow tracking")
	}
}

func TestTrader_HandleSignalTimeoutSettlementPreservesExecutedQuantity(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()
	tr.config.MakerTimeout = 50 * time.Millisecond
	sig := &SpreadSignal{
		Direction:      LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99"),
		TakerBid:       decimal.RequireFromString("101"),
		TakerAsk:       decimal.RequireFromString("102"),
	}

	tr.HandleSignal(sig)

	tr.mu.Lock()
	initialState := tr.state
	tr.mu.Unlock()
	if initialState != StatePlacingMaker && initialState != StateWaitingFill {
		t.Fatalf("state = %s, want placing_maker or waiting_fill", initialState)
	}

	waitForTraderState(t, tr, StateWaitingFill)
	tr.makerOrderCh <- &exchanges.Order{
		OrderID:        "maker-1",
		ClientOrderID:  "cid-1",
		Status:         exchanges.OrderStatusPartiallyFilled,
		FilledQuantity: decimal.RequireFromString("0.001"),
	}
	waitForTraderState(t, tr, StateWaitingFill)
	waitForTraderState(t, tr, StateClosing)

	tr.makerOrderCh <- &exchanges.Order{
		OrderID:        "maker-1",
		ClientOrderID:  "cid-1",
		Status:         exchanges.OrderStatusCancelled,
		FilledQuantity: decimal.RequireFromString("0.001"),
	}

	waitForTraderState(t, tr, StatePositionOpen)
	waitForMakerOrders(t, maker, 1)
	if tr.position == nil {
		t.Fatal("position was not opened")
	}
	if !tr.position.OpenQuantity.Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("open quantity = %s, want 0.001", tr.position.OpenQuantity)
	}

	tr.closePosition("test-close")

	maker.mu.Lock()
	makerQtys := append([]decimal.Decimal(nil), maker.placedQtys...)
	maker.mu.Unlock()
	if len(makerQtys) != 2 {
		t.Fatalf("maker order count = %d, want 2", len(makerQtys))
	}
	if !makerQtys[1].Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("close maker qty = %s, want 0.001", makerQtys[1])
	}

	taker.mu.Lock()
	takerQtys := append([]decimal.Decimal(nil), taker.placedQtys...)
	taker.mu.Unlock()
	if len(takerQtys) != 2 {
		t.Fatalf("taker order count = %d, want 2", len(takerQtys))
	}
	if !takerQtys[1].Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("close taker qty = %s, want 0.001", takerQtys[1])
	}
}
