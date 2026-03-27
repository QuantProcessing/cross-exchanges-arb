package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	placedOrders    []*exchanges.OrderParams
	cancelCalls     int
	cancelAllCalls  int
	lastCancelOrder string
	forcePlaceErr   error
	watchOrdersErr  error
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
	if e.forcePlaceErr != nil {
		return nil, e.forcePlaceErr
	}
	e.placedQtys = append(e.placedQtys, params.Quantity)
	paramsCopy := *params
	e.placedOrders = append(e.placedOrders, &paramsCopy)
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
func (e *testExchange) CancelAllOrders(ctx context.Context, symbol string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelAllCalls++
	return nil
}
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
	if e.watchOrdersErr != nil {
		return e.watchOrdersErr
	}
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

func newTestTrader() *Trader {
	tr, _, _ := newActiveFlowTrader()
	return tr
}

func newTestTraderWithPosition() *Trader {
	tr, _, _ := newActiveFlowTrader()
	tr.engine = &SpreadEngine{}
	tr.position = &ArbPosition{
		Direction:    LongMakerShortTaker,
		OpenTime:     time.Now().Add(-time.Minute),
		OpenQuantity: decimal.RequireFromString("0.001"),
		LongOrder: &exchanges.Order{
			OrderID: "long-open",
		},
		ShortOrder: &exchanges.Order{
			OrderID: "short-open",
		},
		LongExchange:  "maker",
		ShortExchange: "taker",
	}
	tr.state = StatePositionOpen
	return tr
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

func TestTrader_MakerTimeoutWithNoFillsReturnsToIdle(t *testing.T) {
	tr, _, _ := newActiveFlowTrader()

	tr.handleMakerTimeoutForTest()

	// With REST fallback: no fills → reset to idle.
	if tr.state != StateIdle {
		t.Fatalf("state = %s, want idle (REST fallback found no fills)", tr.state)
	}
	if tr.openFlow != nil {
		t.Fatal("openFlow should be nil after no-fill timeout")
	}
}

func TestTrader_HedgeFailureMovesToManualIntervention(t *testing.T) {
	tr := newTestTrader()
	tr.taker.(*testExchange).forcePlaceErr = errors.New("boom")

	_ = tr.handleMakerFillForTest(decimal.RequireFromString("0.001"))

	if tr.state != StateManualIntervention {
		t.Fatalf("state = %s, want manual_intervention", tr.state)
	}
	if tr.openFlow == nil || tr.openFlow.signal == nil {
		t.Fatal("open-flow context was cleared, want residual position context for alerting")
	}
	if CanAcceptSignal(tr.state) {
		t.Fatal("manual intervention must reject new signals")
	}
}

func TestTrader_CloseFailureBlocksNextRound(t *testing.T) {
	tr := newTestTraderWithPosition()
	tr.taker.(*testExchange).forcePlaceErr = errors.New("close failed")

	tr.closePosition("test")

	if tr.state == StateIdle {
		t.Fatal("close failure must not return to idle")
	}
	if tr.position == nil {
		t.Fatal("close failure must preserve the open position for intervention")
	}
	if tr.position.LongOrder != nil {
		t.Fatal("close failure must clear the closed long leg from memory")
	}
	if tr.position.ShortOrder == nil {
		t.Fatal("close failure must retain the surviving short leg context")
	}
	tr.HandleSignal(&SpreadSignal{
		Direction:      LongMakerShortTaker,
		SpreadBps:      9.1,
		ZScore:         2.2,
		ExpectedProfit: 3.3,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99"),
		TakerBid:       decimal.RequireFromString("101"),
		TakerAsk:       decimal.RequireFromString("102"),
	})
	if tr.state != StateManualIntervention {
		t.Fatalf("new signals must remain blocked after close failure, state = %s", tr.state)
	}
}

func TestTrader_SuccessfulCloseTransitionsToCooldown(t *testing.T) {
	tr := newTestTraderWithPosition()

	tr.closePosition("done")

	if tr.state != StateCooldown {
		t.Fatalf("state = %s, want cooldown", tr.state)
	}
	if tr.completedRounds != 1 {
		t.Fatalf("completedRounds = %d, want 1", tr.completedRounds)
	}
}

func TestTrader_MaxRoundsStopsNewTrading(t *testing.T) {
	tr := newTestTrader()
	tr.completedRounds = 1
	tr.config.LiveValidate = true
	tr.config.MaxRounds = 1

	if tr.canStartNextRound() {
		t.Fatal("expected trading to stop after max rounds")
	}
}

func TestTrader_MaxRoundsDoesNotBlockDryRun(t *testing.T) {
	tr := newTestTrader()
	tr.completedRounds = 1
	tr.config.DryRun = true
	tr.config.LiveValidate = true
	tr.config.MaxRounds = 1

	if !tr.canStartNextRound() {
		t.Fatal("expected dry-run trading to bypass max-round gating")
	}
}

func TestTrader_MaxRoundsDoesNotBlockWhenLiveValidationDisabled(t *testing.T) {
	tr := newTestTrader()
	tr.completedRounds = 1
	tr.config.DryRun = false
	tr.config.LiveValidate = false
	tr.config.MaxRounds = 1

	if !tr.canStartNextRound() {
		t.Fatal("expected non-live trading to bypass max-round gating")
	}
}

func TestTrader_CooldownExpiryReturnsToIdle(t *testing.T) {
	tr := newTestTraderWithPosition()
	tr.config.Cooldown = 100 * time.Millisecond

	tr.closePosition("done")

	tr.mu.Lock()
	tr.lastTrade = time.Now().Add(-tr.config.Cooldown - time.Millisecond)
	tr.state = StateCooldown
	tr.mu.Unlock()

	if !tr.canStartNextRound() {
		t.Fatal("expected trading to resume after cooldown expires")
	}

	if tr.state != StateIdle {
		t.Fatalf("state = %s, want idle", tr.state)
	}
}

func TestTrader_CompletedRoundsIncrementOncePerResolvedClose(t *testing.T) {
	tr := newTestTraderWithPosition()

	tr.closePosition("done")
	tr.closePosition("done-again")

	if tr.completedRounds != 1 {
		t.Fatalf("completedRounds = %d, want 1", tr.completedRounds)
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
	// Maker partial fill triggers hedge → waitTakerFill reads from takerOrderCh
	go func() {
		time.Sleep(50 * time.Millisecond)
		tr.takerOrderCh <- &exchanges.Order{
			OrderID:        "taker-1",
			Status:         exchanges.OrderStatusFilled,
			FilledQuantity: decimal.RequireFromString("0.001"),
		}
	}()
	tr.makerOrderCh <- &exchanges.Order{
		OrderID:        "maker-1",
		ClientOrderID:  "cid-1",
		Status:         exchanges.OrderStatusPartiallyFilled,
		FilledQuantity: decimal.RequireFromString("0.001"),
	}
	waitForTraderState(t, tr, StateWaitingFill)
	waitForTraderState(t, tr, StateClosing)

	// Maker cancel with fills also triggers hedge → provide taker fill
	go func() {
		time.Sleep(50 * time.Millisecond)
		tr.takerOrderCh <- &exchanges.Order{
			OrderID:        "taker-2",
			Status:         exchanges.OrderStatusFilled,
			FilledQuantity: decimal.RequireFromString("0.001"),
		}
	}()
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

func TestTrader_GracefulShutdownClosesOpenPosition(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()
	tr.position = &ArbPosition{
		Direction:    LongMakerShortTaker,
		OpenTime:     time.Now().Add(-time.Minute),
		OpenQuantity: decimal.RequireFromString("0.001"),
		LongExchange: "maker",
		ShortExchange:"taker",
	}
	tr.state = StatePositionOpen

	tr.GracefulShutdown(2 * time.Second)

	if tr.position != nil {
		t.Fatal("position should be closed during graceful shutdown")
	}
	if tr.state != StateCooldown {
		t.Fatalf("state = %s, want cooldown", tr.state)
	}

	maker.mu.Lock()
	makerOrders := append([]*exchanges.OrderParams(nil), maker.placedOrders...)
	makerCancelAll := maker.cancelAllCalls
	maker.mu.Unlock()
	taker.mu.Lock()
	takerOrders := append([]*exchanges.OrderParams(nil), taker.placedOrders...)
	takerCancelAll := taker.cancelAllCalls
	taker.mu.Unlock()

	if makerCancelAll != 1 {
		t.Fatalf("maker cancel all calls = %d, want 1", makerCancelAll)
	}
	if takerCancelAll != 1 {
		t.Fatalf("taker cancel all calls = %d, want 1", takerCancelAll)
	}
	if len(makerOrders) != 1 {
		t.Fatalf("maker close orders = %d, want 1", len(makerOrders))
	}
	if len(takerOrders) != 1 {
		t.Fatalf("taker close orders = %d, want 1", len(takerOrders))
	}
	if !makerOrders[0].ReduceOnly {
		t.Fatal("maker shutdown close must be reduce-only")
	}
	if !takerOrders[0].ReduceOnly {
		t.Fatal("taker shutdown close must be reduce-only")
	}
}

func TestTrader_StartFailsWhenWatchOrdersSubscriptionFails(t *testing.T) {
	tr, maker, _ := newActiveFlowTrader()
	maker.watchOrdersErr = errors.New("watch orders auth failed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := tr.Start(ctx)
	if err == nil {
		t.Fatal("expected start to fail")
	}
	if !strings.Contains(err.Error(), "maker") {
		t.Fatalf("error = %v, want maker context", err)
	}
}

func TestTrader_StartSucceedsWhenWatchOrdersStayActive(t *testing.T) {
	tr := newTestTrader()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := tr.Start(ctx); err != nil {
		t.Fatalf("start error = %v, want nil", err)
	}
}
