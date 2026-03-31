package trading

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

type testExchange struct {
	mu               sync.Mutex
	name             string
	symbolDetails    *exchanges.SymbolDetails
	placedQtys       []decimal.Decimal
	placedOrders     []*exchanges.OrderParams
	cancelCalls      int
	cancelAllCalls   int
	lastCancelOrder  string
	forcePlaceErr    error
	watchOrdersErr   error
	watchOrdersAsync bool
	queuedOrders     []*exchanges.Order
	ordersByID       map[string]*exchanges.Order
	fetchOrderErr    error
}

type testEngine struct {
	mu              sync.Mutex
	snapshot        spread.Snapshot
	roundTripFeeBps float64
	currentZAB      float64
	currentZBA      float64
	makerFee        spread.FeeInfo
	takerFee        spread.FeeInfo
	onMarketUpdate  func()
}

func (e *testEngine) SetMarketUpdateCallback(cb func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onMarketUpdate = cb
}

func (e *testEngine) Snapshot() *spread.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := e.snapshot
	return &snapshot
}

func (e *testEngine) RoundTripFeeBps() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.roundTripFeeBps
}

func (e *testEngine) CurrentZ() (float64, float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentZAB, e.currentZBA
}

func (e *testEngine) Fees() (spread.FeeInfo, spread.FeeInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.makerFee, e.takerFee
}

func (e *testEngine) setSnapshot(snapshot spread.Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snapshot = snapshot
}

func (e *testEngine) emitMarketUpdate() {
	e.mu.Lock()
	cb := e.onMarketUpdate
	e.mu.Unlock()
	if cb != nil {
		cb()
	}
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
	order := &exchanges.Order{
		OrderID:        fmt.Sprintf("%s-%d", e.name, len(e.placedQtys)),
		Symbol:         params.Symbol,
		Side:           params.Side,
		Type:           params.Type,
		Quantity:       params.Quantity,
		Status:         exchanges.OrderStatusFilled,
		FilledQuantity: params.Quantity,
		ClientOrderID:  params.ClientID,
	}
	if len(e.queuedOrders) > 0 && e.queuedOrders[0] != nil {
		order = cloneTestOrder(e.queuedOrders[0])
		e.queuedOrders = e.queuedOrders[1:]
		if order.OrderID == "" {
			order.OrderID = fmt.Sprintf("%s-%d", e.name, len(e.placedQtys))
		}
		if order.Symbol == "" {
			order.Symbol = params.Symbol
		}
		if order.Side == "" {
			order.Side = params.Side
		}
		if order.Type == "" {
			order.Type = params.Type
		}
		if order.Quantity.IsZero() {
			order.Quantity = params.Quantity
		}
		if order.ClientOrderID == "" {
			order.ClientOrderID = params.ClientID
		}
		if order.Status == "" {
			order.Status = exchanges.OrderStatusFilled
		}
		if order.Status == exchanges.OrderStatusFilled && order.FilledQuantity.IsZero() {
			order.FilledQuantity = params.Quantity
		}
	}
	return order, nil
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fetchOrderErr != nil {
		return nil, e.fetchOrderErr
	}
	if e.ordersByID == nil {
		return nil, nil
	}
	order := e.ordersByID[orderID]
	if order == nil {
		return nil, nil
	}
	return cloneTestOrder(order), nil
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
	if e.watchOrdersAsync {
		return nil
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
		config: &appconfig.Config{
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
		signal: &spread.Signal{
			Direction:      spread.LongMakerShortTaker,
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
	tr.engine = &testEngine{}
	tr.position = &ArbPosition{
		Direction:          spread.LongMakerShortTaker,
		OpenTime:           time.Now().Add(-time.Minute),
		OpenQuantity:       decimal.RequireFromString("0.001"),
		OpenExpectedProfit: 4.2,
		LongOrder: &exchanges.Order{
			OrderID: "long-open",
			Price:   decimal.RequireFromString("100"),
		},
		ShortOrder: &exchanges.Order{
			OrderID: "short-open",
			Price:   decimal.RequireFromString("101"),
		},
		LongExchange:  "maker",
		ShortExchange: "taker",
	}
	tr.state = StatePositionOpen
	return tr
}

func cloneTestOrder(order *exchanges.Order) *exchanges.Order {
	if order == nil {
		return nil
	}
	clone := *order
	return &clone
}

func waitForTraderState(t *testing.T, tr *Trader, want State) {
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

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal(msg)
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

func TestTrader_CancelsPendingMakerWhenEntryEdgeDisappears(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()
	tr.config.MakerTimeout = 2 * time.Second
	tr.config.MinProfitBps = 1.0
	engine := &testEngine{}
	engine.setSnapshot(spread.Snapshot{
		MakerBid: decimal.RequireFromString("100"),
		MakerAsk: decimal.RequireFromString("100.10"),
		TakerBid: decimal.RequireFromString("99.90"),
		TakerAsk: decimal.RequireFromString("100.00"),
		MeanAB:   0,
		MeanBA:   0,
	})
	tr.engine = engine

	sig := &spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99.95"),
		TakerBid:       decimal.RequireFromString("100.15"),
		TakerAsk:       decimal.RequireFromString("100.20"),
	}

	tr.HandleSignal(sig)

	waitForCondition(t, 400*time.Millisecond, func() bool {
		maker.mu.Lock()
		defer maker.mu.Unlock()
		return maker.cancelCalls > 0
	}, "expected stale maker order to be cancelled before maker-timeout")

	tr.makerOrderCh <- &exchanges.Order{
		OrderID:        "maker-1",
		ClientOrderID:  "cid-1",
		Status:         exchanges.OrderStatusCancelled,
		FilledQuantity: decimal.Zero,
	}

	waitForTraderState(t, tr, StateIdle)
	if tr.openFlow != nil {
		t.Fatal("openFlow should be cleared after stale maker cancel")
	}

	taker.mu.Lock()
	defer taker.mu.Unlock()
	if len(taker.placedQtys) != 0 {
		t.Fatalf("taker hedge orders = %d, want 0", len(taker.placedQtys))
	}
}

func TestTrader_CancelsPendingMakerOnImmediateMarketUpdate(t *testing.T) {
	tr, maker, _ := newActiveFlowTrader()
	tr.config.MakerTimeout = 2 * time.Second
	tr.config.MinProfitBps = 1.0
	engine := &testEngine{}
	engine.setSnapshot(spread.Snapshot{MeanAB: 0, MeanBA: 0})
	tr.engine = engine

	sig := &spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99.95"),
		TakerBid:       decimal.RequireFromString("100.15"),
		TakerAsk:       decimal.RequireFromString("100.20"),
	}

	tr.HandleSignal(sig)
	waitForTraderState(t, tr, StateWaitingFill)

	engine.setSnapshot(spread.Snapshot{
		MakerBid: decimal.RequireFromString("100"),
		MakerAsk: decimal.RequireFromString("100.10"),
		TakerBid: decimal.RequireFromString("99.90"),
		TakerAsk: decimal.RequireFromString("100.00"),
		MeanAB:   0,
		MeanBA:   0,
	})
	engine.emitMarketUpdate()

	waitForCondition(t, 80*time.Millisecond, func() bool {
		maker.mu.Lock()
		defer maker.mu.Unlock()
		return maker.cancelCalls > 0
	}, "expected stale maker order to be cancelled immediately after market update")
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
	tr.HandleSignal(&spread.Signal{
		Direction:      spread.LongMakerShortTaker,
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
	sig := &spread.Signal{
		Direction:      spread.LongMakerShortTaker,
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
		Direction:     spread.LongMakerShortTaker,
		OpenTime:      time.Now().Add(-time.Minute),
		OpenQuantity:  decimal.RequireFromString("0.001"),
		LongExchange:  "maker",
		ShortExchange: "taker",
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

func TestTrader_StartSucceedsWhenWatchOrdersRegistersAsync(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()
	maker.watchOrdersAsync = true
	taker.watchOrdersAsync = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := tr.Start(ctx); err != nil {
		t.Fatalf("start error = %v, want nil", err)
	}
}

func TestTrader_FinalizeOpenFlowAssignsLongShortOrdersByDirection(t *testing.T) {
	tr, _, _ := newActiveFlowTrader()
	tr.openFlow.signal.Direction = spread.LongTakerShortMaker
	tr.openFlow.lastHedgeOrder = &exchanges.Order{
		OrderID: "taker-open",
		Price:   decimal.RequireFromString("100"),
	}
	makerOrder := &exchanges.Order{
		OrderID: "maker-open",
		Price:   decimal.RequireFromString("101"),
	}

	tr.finalizeOpenFlow(makerOrder, decimal.RequireFromString("0.003"))

	if tr.position == nil {
		t.Fatal("position should be set")
	}
	if tr.position.LongExchange != "taker" {
		t.Fatalf("long exchange = %s, want taker", tr.position.LongExchange)
	}
	if tr.position.ShortExchange != "maker" {
		t.Fatalf("short exchange = %s, want maker", tr.position.ShortExchange)
	}
	if tr.position.LongOrder == nil || tr.position.LongOrder.OrderID != "taker-open" {
		t.Fatalf("long order = %#v, want taker hedge order", tr.position.LongOrder)
	}
	if tr.position.ShortOrder == nil || tr.position.ShortOrder.OrderID != "maker-open" {
		t.Fatalf("short order = %#v, want maker order", tr.position.ShortOrder)
	}
}

func TestCalculateRealizedProfitMetrics_LongMakerShortTaker(t *testing.T) {
	cfg := &appconfig.Config{
		MakerExchange: "maker",
		TakerExchange: "taker",
	}
	pos := &ArbPosition{
		Direction:     spread.LongMakerShortTaker,
		LongExchange:  "maker",
		ShortExchange: "taker",
		LongOrder: &exchanges.Order{
			OrderID: "long-open",
			Price:   decimal.RequireFromString("100"),
		},
		ShortOrder: &exchanges.Order{
			OrderID: "short-open",
			Price:   decimal.RequireFromString("101"),
		},
	}
	closeLong := &exchanges.Order{
		OrderID: "long-close",
		Price:   decimal.RequireFromString("100.5"),
	}
	closeShort := &exchanges.Order{
		OrderID: "short-close",
		Price:   decimal.RequireFromString("100.2"),
	}

	metrics, err := calculateRealizedProfitMetrics(pos, cfg,
		spread.FeeInfo{MakerRate: 0.0001, TakerRate: 0.0003},
		spread.FeeInfo{MakerRate: 0.0002, TakerRate: 0.0004},
		closeLong, closeShort,
	)
	if err != nil {
		t.Fatalf("calculateRealizedProfitMetrics error = %v", err)
	}

	if diff := math.Abs(metrics.EntryBps - 99.50248756218905); diff > 1e-9 {
		t.Fatalf("entry bps = %.12f, want %.12f", metrics.EntryBps, 99.50248756218905)
	}
	if diff := math.Abs(metrics.ExitBps - 29.895366218235893); diff > 1e-9 {
		t.Fatalf("exit bps = %.12f, want %.12f", metrics.ExitBps, 29.895366218235893)
	}
	if diff := math.Abs(metrics.GrossBps - 129.449838187702); diff > 1e-9 {
		t.Fatalf("gross bps = %.12f, want %.12f", metrics.GrossBps, 129.449838187702)
	}
	if diff := math.Abs(metrics.FeeBps - 12.011949215832713); diff > 1e-9 {
		t.Fatalf("fee bps = %.12f, want %.12f", metrics.FeeBps, 12.011949215832713)
	}
	if diff := math.Abs(metrics.NetBps - 117.43788897186927); diff > 1e-9 {
		t.Fatalf("net bps = %.12f, want %.12f", metrics.NetBps, 117.43788897186927)
	}
}

func TestTrader_VerifyCloseLegUsesFetchFallback(t *testing.T) {
	tr := newTestTraderWithPosition()
	maker := tr.maker.(*testExchange)
	maker.ordersByID = map[string]*exchanges.Order{
		"maker-close": {
			OrderID:        "maker-close",
			Status:         exchanges.OrderStatusFilled,
			Price:          decimal.RequireFromString("100.1"),
			FilledQuantity: decimal.RequireFromString("0.001"),
		},
	}

	prevTimeout := closeLegVerifyTimeout
	closeLegVerifyTimeout = 10 * time.Millisecond
	defer func() {
		closeLegVerifyTimeout = prevTimeout
	}()

	ok, err := tr.verifyCloseLeg(context.Background(), &exchanges.Order{
		OrderID: "maker-close",
		Status:  exchanges.OrderStatusPending,
	}, tr.makerOrderCh, tr.takerOrderCh, true)
	if err != nil {
		t.Fatalf("verifyCloseLeg error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("verifyCloseLeg = false, want true")
	}
}

func TestTrader_VerifyCloseLegFailsWhenFetchCannotConfirm(t *testing.T) {
	tr := newTestTraderWithPosition()
	maker := tr.maker.(*testExchange)
	maker.fetchOrderErr = errors.New("fetch failed")

	prevTimeout := closeLegVerifyTimeout
	closeLegVerifyTimeout = 10 * time.Millisecond
	defer func() {
		closeLegVerifyTimeout = prevTimeout
	}()

	ok, err := tr.verifyCloseLeg(context.Background(), &exchanges.Order{
		OrderID: "maker-close",
		Status:  exchanges.OrderStatusPending,
	}, tr.makerOrderCh, tr.takerOrderCh, true)
	if err == nil {
		t.Fatal("verifyCloseLeg error = nil, want error")
	}
	if ok {
		t.Fatal("verifyCloseLeg = true, want false")
	}
}
