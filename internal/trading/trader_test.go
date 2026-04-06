package trading

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/runlog"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/exchanges/account"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type testExchange struct {
	mu                sync.Mutex
	name              string
	symbolDetails     *exchanges.SymbolDetails
	balance           decimal.Decimal
	placedQtys        []decimal.Decimal
	placedOrders      []*exchanges.OrderParams
	cancelCalls       int
	cancelAllCalls    int
	lastCancelOrder   string
	forcePlaceErr     error
	fetchBalanceErr   error
	watchOrdersErr    error
	watchPositionsErr error
	watchFillsErr     error
	queuedOrders      []*exchanges.Order
	queuedFills       []*exchanges.Fill
	ordersByID        map[string]*exchanges.Order
	fetchOrderErr     error
	accountSnapshot   *exchanges.Account
	watchOrdersCB     exchanges.OrderUpdateCallback
	watchFillsCB      exchanges.FillCallback
	watchPositionsCB  exchanges.PositionUpdateCallback
	placeWSCalls      int
	cancelWSCalls     int
	autoEmitWS        bool
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
func (e *testExchange) PlaceOrderWS(ctx context.Context, params *exchanges.OrderParams) error {
	e.mu.Lock()
	if e.forcePlaceErr != nil {
		e.mu.Unlock()
		return e.forcePlaceErr
	}
	e.placeWSCalls++
	e.placedQtys = append(e.placedQtys, params.Quantity)
	paramsCopy := *params
	e.placedOrders = append(e.placedOrders, &paramsCopy)
	shouldEmit := e.autoEmitWS
	orderID := fmt.Sprintf("%s-ws-%d", e.name, e.placeWSCalls)
	if e.ordersByID == nil {
		e.ordersByID = make(map[string]*exchanges.Order)
	}
	order := &exchanges.Order{
		OrderID:        orderID,
		ClientOrderID:  params.ClientID,
		Symbol:         params.Symbol,
		Side:           params.Side,
		Type:           params.Type,
		Quantity:       params.Quantity,
		Status:         exchanges.OrderStatusFilled,
		FilledQuantity: params.Quantity,
		ReduceOnly:     params.ReduceOnly,
	}
	if params.Type == exchanges.OrderTypePostOnly && !params.ReduceOnly {
		order.Status = exchanges.OrderStatusPending
		order.FilledQuantity = decimal.Zero
	}
	e.ordersByID[orderID] = cloneTestOrder(order)
	cb := e.watchOrdersCB
	fillCB := e.watchFillsCB
	e.mu.Unlock()
	if shouldEmit && cb != nil {
		go cb(cloneTestOrder(order))
	}
	if shouldEmit && fillCB != nil && order.Status == exchanges.OrderStatusFilled && order.FilledQuantity.GreaterThan(decimal.Zero) {
		go fillCB(&exchanges.Fill{
			TradeID:       fmt.Sprintf("%s-fill-%d", e.name, e.placeWSCalls),
			OrderID:       order.OrderID,
			ClientOrderID: order.ClientOrderID,
			Symbol:        order.Symbol,
			Side:          order.Side,
			Price:         order.Price,
			Quantity:      order.FilledQuantity,
			Timestamp:     time.Now().UnixMilli(),
		})
	}
	return nil
}
func (e *testExchange) CancelOrder(ctx context.Context, orderID, symbol string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelCalls++
	e.lastCancelOrder = orderID
	return nil
}
func (e *testExchange) CancelOrderWS(ctx context.Context, orderID, symbol string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelWSCalls++
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
func (e *testExchange) FetchAccount(ctx context.Context) (*exchanges.Account, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.accountSnapshot != nil {
		snapshot := *e.accountSnapshot
		return &snapshot, nil
	}
	return &exchanges.Account{}, nil
}
func (e *testExchange) FetchBalance(ctx context.Context) (decimal.Decimal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fetchBalanceErr != nil {
		return decimal.Zero, e.fetchBalanceErr
	}
	return e.balance, nil
}
func (e *testExchange) FetchSymbolDetails(ctx context.Context, symbol string) (*exchanges.SymbolDetails, error) {
	return e.symbolDetails, nil
}
func (e *testExchange) FetchFeeRate(ctx context.Context, symbol string) (*exchanges.FeeRate, error) {
	return nil, nil
}
func (e *testExchange) WatchOrderBook(ctx context.Context, symbol string, depth int, cb exchanges.OrderBookCallback) error {
	return nil
}
func (e *testExchange) GetLocalOrderBook(symbol string, depth int) *exchanges.OrderBook { return nil }
func (e *testExchange) StopWatchOrderBook(ctx context.Context, symbol string) error     { return nil }
func (e *testExchange) WatchOrders(ctx context.Context, cb exchanges.OrderUpdateCallback) error {
	e.mu.Lock()
	e.watchOrdersCB = cb
	e.mu.Unlock()
	if e.watchOrdersErr != nil {
		return e.watchOrdersErr
	}
	return nil
}
func (e *testExchange) WatchFills(ctx context.Context, cb exchanges.FillCallback) error {
	e.mu.Lock()
	e.watchFillsCB = cb
	e.mu.Unlock()
	if e.watchFillsErr != nil {
		return e.watchFillsErr
	}
	for _, fill := range e.queuedFills {
		if cb != nil {
			cb(fill)
		}
	}
	return nil
}
func (e *testExchange) WatchPositions(ctx context.Context, cb exchanges.PositionUpdateCallback) error {
	e.mu.Lock()
	e.watchPositionsCB = cb
	e.mu.Unlock()
	if e.watchPositionsErr != nil {
		return e.watchPositionsErr
	}
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

func (e *testExchange) emitOrderUpdate(order *exchanges.Order) {
	e.mu.Lock()
	cb := e.watchOrdersCB
	e.mu.Unlock()
	if cb != nil {
		cb(order)
	}
}

func (e *testExchange) emitPositionUpdate(position *exchanges.Position) {
	e.mu.Lock()
	cb := e.watchPositionsCB
	e.mu.Unlock()
	if cb != nil {
		cb(position)
	}
}

func (e *testExchange) emitFill(fill *exchanges.Fill) {
	e.mu.Lock()
	cb := e.watchFillsCB
	e.mu.Unlock()
	if cb != nil {
		cb(fill)
	}
}

func newActiveFlowTrader() (*Trader, *testExchange, *testExchange) {
	maker := newTestExchange("maker")
	taker := newTestExchange("taker")
	qty := decimal.RequireFromString("0.003")
	makerAccount := account.NewTradingAccount(maker, exchanges.NopLogger)
	takerAccount := account.NewTradingAccount(taker, exchanges.NopLogger)

	tr := &Trader{
		maker:        maker,
		taker:        taker,
		makerAccount: makerAccount,
		takerAccount: takerAccount,
		config: &appconfig.Config{
			MakerExchange: "maker",
			TakerExchange: "taker",
			Symbol:        "BTC",
			Quantity:      qty,
			Slippage:      0.001,
			MakerTimeout:  1 * time.Second,
			MaxRounds:     1,
		},
		logger:         zap.NewNop().Sugar(),
		makerOrderCh:   make(chan *exchanges.Order, 10),
		takerOrderCh:   make(chan *exchanges.Order, 10),
		marketUpdateCh: make(chan struct{}, 1),
		orderTraces:    make(map[string]*orderTrace),
		state:          StateIdle,
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

	_ = makerAccount.Start(context.Background())
	_ = takerAccount.Start(context.Background())

	return tr, maker, taker
}

func newTestTrader() *Trader {
	tr, _, _ := newActiveFlowTrader()
	return tr
}

func newTestTraderWithPosition() *Trader {
	tr, maker, taker := newActiveFlowTrader()
	maker.autoEmitWS = true
	taker.autoEmitWS = true
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
	maker.autoEmitWS = true
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
	waitForCondition(t, 500*time.Millisecond, func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.openFlow != nil && tr.openFlow.makerOrder != nil && tr.openFlow.makerOrder.ClientOrderID != ""
	}, "expected openFlow maker client id to be populated")
	makerClientID := ""
	tr.mu.Lock()
	if tr.openFlow != nil && tr.openFlow.makerOrder != nil {
		makerClientID = tr.openFlow.makerOrder.ClientOrderID
	}
	tr.mu.Unlock()

	waitForCondition(t, 400*time.Millisecond, func() bool {
		maker.mu.Lock()
		defer maker.mu.Unlock()
		return maker.cancelWSCalls > 0
	}, "expected stale maker order to be cancelled before maker-timeout")
	maker.mu.Lock()
	cancelOrderID := maker.lastCancelOrder
	maker.mu.Unlock()

	maker.emitOrderUpdate(&exchanges.Order{
		OrderID:        cancelOrderID,
		ClientOrderID:  makerClientID,
		Status:         exchanges.OrderStatusCancelled,
		FilledQuantity: decimal.Zero,
	})

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
	maker.autoEmitWS = true
	tr.config.MakerTimeout = 2 * time.Second
	tr.config.MinProfitBps = 1.0
	tr.config.ZOpen = 2
	engine := &testEngine{}
	engine.setSnapshot(spread.Snapshot{
		MakerBid:    decimal.RequireFromString("99.95"),
		MakerAsk:    decimal.RequireFromString("100"),
		TakerBid:    decimal.RequireFromString("100.15"),
		TakerAsk:    decimal.RequireFromString("100.20"),
		MakerAskQty: decimal.RequireFromString("1"),
		TakerBidQty: decimal.RequireFromString("1"),
		MakerBidQty: decimal.RequireFromString("1"),
		TakerAskQty: decimal.RequireFromString("1"),
		SpreadAB:    14.9888,
		MeanAB:      10,
		StdDevAB:    1,
		ValidAB:     true,
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
		return maker.cancelWSCalls > 0
	}, "expected stale maker order to be cancelled immediately after market update")
}

func TestTrader_CancelPendingMakerWaitsForResolvableOrderID(t *testing.T) {
	tr, maker, _ := newActiveFlowTrader()
	flow, err := tr.makerAccount.Track("", "cid-resolve")
	if err != nil {
		t.Fatalf("Track() error = %v", err)
	}
	tr.state = StateWaitingFill
	tr.openFlow = &openFlowState{
		signal: &spread.Signal{Direction: spread.LongMakerShortTaker},
		makerOrder: &exchanges.Order{
			ClientOrderID: "cid-resolve",
		},
		makerFlow: flow,
		makerQty:  decimal.RequireFromString("0.003"),
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		update := &exchanges.Order{
			OrderID:       "maker-resolved",
			ClientOrderID: "cid-resolve",
			Status:        exchanges.OrderStatusCancelled,
		}
		maker.emitOrderUpdate(update)
	}()

	if err := tr.cancelPendingMaker(context.Background(), "test"); err != nil {
		t.Fatalf("cancelPendingMaker() error = %v", err)
	}

	maker.mu.Lock()
	lastCancelOrder := maker.lastCancelOrder
	maker.mu.Unlock()
	if lastCancelOrder != "maker-resolved" {
		t.Fatalf("lastCancelOrder = %q, want maker-resolved", lastCancelOrder)
	}
	if tr.state != StateIdle {
		t.Fatalf("state = %s, want idle", tr.state)
	}
}

func TestTrader_HedgeFailureMovesToManualIntervention(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	tr := newTestTrader()
	tr.logger = zap.New(core).Sugar()
	tr.taker.(*testExchange).forcePlaceErr = errors.New("boom")
	wantQty := decimal.RequireFromString("0.001")

	_ = tr.handleMakerFillForTest(wantQty)

	if tr.state != StateManualIntervention {
		t.Fatalf("state = %s, want manual_intervention", tr.state)
	}
	if tr.openFlow == nil || tr.openFlow.signal == nil {
		t.Fatal("open-flow context was cleared, want residual position context for alerting")
	}
	if CanAcceptSignal(tr.state) {
		t.Fatal("manual intervention must reject new signals")
	}
	hedgeFailedLogs := observed.FilterMessageSnippet("EVT hedge_failed").All()
	if len(hedgeFailedLogs) != 1 {
		t.Fatalf("hedge_failed event logs = %d, want 1", len(hedgeFailedLogs))
	}
	if !strings.Contains(hedgeFailedLogs[0].Message, "qty="+wantQty.String()) {
		t.Fatalf("hedge_failed log missing qty: %s", hedgeFailedLogs[0].Message)
	}

	manualLogs := observed.FilterMessageSnippet("EVT manual_intervention").All()
	if len(manualLogs) != 1 {
		t.Fatalf("manual_intervention event logs = %d, want 1", len(manualLogs))
	}
	if !strings.Contains(manualLogs[0].Message, `reason="hedge failed for qty=`) {
		t.Fatalf("manual_intervention log missing hedge reason: %s", manualLogs[0].Message)
	}
}

func TestTrader_CloseFailureBlocksNextRound(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	tr := newTestTraderWithPosition()
	tr.logger = zap.New(core).Sugar()
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
	closeFailedLogs := observed.FilterMessageSnippet("EVT close_failed").All()
	if len(closeFailedLogs) != 1 {
		t.Fatalf("close_failed event logs = %d, want 1", len(closeFailedLogs))
	}
	if !strings.Contains(closeFailedLogs[0].Message, "residual=short") || !strings.Contains(closeFailedLogs[0].Message, "detail=") {
		t.Fatalf("close_failed log missing residual/detail context: %s", closeFailedLogs[0].Message)
	}

	manualLogs := observed.FilterMessageSnippet("EVT manual_intervention").All()
	if len(manualLogs) != 1 {
		t.Fatalf("manual_intervention event logs = %d, want 1", len(manualLogs))
	}
	if !strings.Contains(manualLogs[0].Message, `reason="close failed residual=short"`) {
		t.Fatalf("manual_intervention log missing close reason: %s", manualLogs[0].Message)
	}
}

func TestTrader_CloseRequiresTerminalConfirmation(t *testing.T) {
	tr := newTestTraderWithPosition()
	maker := tr.maker.(*testExchange)
	taker := tr.taker.(*testExchange)
	maker.autoEmitWS = false
	taker.autoEmitWS = false

	prevTimeout := closeLegVerifyTimeout
	closeLegVerifyTimeout = 10 * time.Millisecond
	defer func() {
		closeLegVerifyTimeout = prevTimeout
	}()

	tr.closePosition("need-confirmation")

	if tr.state != StateManualIntervention {
		t.Fatalf("state = %s, want manual_intervention", tr.state)
	}
	if tr.position == nil {
		t.Fatal("position should be preserved when close legs are unconfirmed")
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
	tr.config.MaxRounds = 1

	if tr.canStartNextRound() {
		t.Fatal("expected trading to stop after max rounds")
	}
}

func TestTrader_MaxRoundsDoesNotBlockWhenBelowLimit(t *testing.T) {
	tr := newTestTrader()
	tr.completedRounds = 0
	tr.config.MaxRounds = 1

	if !tr.canStartNextRound() {
		t.Fatal("expected trading to continue when completed rounds are below the limit")
	}
}

func TestTrader_CooldownExpiryReturnsToIdle(t *testing.T) {
	tr := newTestTraderWithPosition()
	tr.config.Cooldown = 100 * time.Millisecond
	tr.config.MaxRounds = 2

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

func TestTrader_CloseUsesExecutedOpenQuantityAfterPartialFill(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()
	tr.config.MakerTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
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
	makerClientID := ""
	tr.mu.Lock()
	if tr.openFlow != nil && tr.openFlow.makerOrder != nil {
		makerClientID = tr.openFlow.makerOrder.ClientOrderID
	}
	tr.mu.Unlock()
	maker.emitOrderUpdate(&exchanges.Order{
		ClientOrderID:  makerClientID,
		Status:         exchanges.OrderStatusPartiallyFilled,
		FilledQuantity: decimal.RequireFromString("0.001"),
	})
	waitForTraderState(t, tr, StateWaitingFill)
	tr.finalizeOpenFlow(tr.openFlow.makerOrder, decimal.RequireFromString("0.001"))
	waitForMakerOrders(t, maker, 1)
	if tr.position == nil {
		t.Fatal("position was not opened")
	}
	if !tr.position.OpenQuantity.Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("open quantity = %s, want 0.001", tr.position.OpenQuantity)
	}

	maker.autoEmitWS = true
	taker.autoEmitWS = true
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
	if len(takerQtys) != 1 {
		t.Fatalf("taker order count = %d, want 1 close order", len(takerQtys))
	}
	if !takerQtys[0].Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("close taker qty = %s, want 0.001", takerQtys[0])
	}
}

func TestTrader_OpenUsesSignalQuantityWhenProvided(t *testing.T) {
	tr, maker, _ := newActiveFlowTrader()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	sig := &spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99"),
		TakerBid:       decimal.RequireFromString("101"),
		TakerAsk:       decimal.RequireFromString("102"),
		Quantity:       decimal.RequireFromString("0.002"),
	}

	tr.HandleSignal(sig)
	waitForTraderState(t, tr, StateWaitingFill)

	maker.mu.Lock()
	defer maker.mu.Unlock()
	if len(maker.placedQtys) != 1 {
		t.Fatalf("maker order count = %d, want 1", len(maker.placedQtys))
	}
	if !maker.placedQtys[0].Equal(decimal.RequireFromString("0.002")) {
		t.Fatalf("maker placed qty = %s, want 0.002", maker.placedQtys[0])
	}
}

func TestTrader_HandleSignalLogsEVTSignalAndMakerPlaced(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()

	tr, maker, _ := newActiveFlowTrader()
	tr.logger = logger
	maker.autoEmitWS = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	tr.HandleSignal(&spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99"),
		TakerBid:       decimal.RequireFromString("101"),
		TakerAsk:       decimal.RequireFromString("102"),
	})

	waitForTraderState(t, tr, StateWaitingFill)

	if observed.FilterMessageSnippet("EVT signal").Len() != 1 {
		t.Fatalf("signal event logs = %d, want 1", observed.FilterMessageSnippet("EVT signal").Len())
	}
	if observed.FilterMessageSnippet("EVT maker_placed").Len() != 1 {
		t.Fatalf("maker_placed event logs = %d, want 1", observed.FilterMessageSnippet("EVT maker_placed").Len())
	}
}

func TestTrader_OpenHedgesFromMakerFlowFillWithoutOrderUpdate(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()

	tr, maker, taker := newActiveFlowTrader()
	tr.logger = logger
	maker.autoEmitWS = true
	taker.autoEmitWS = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	tr.HandleSignal(&spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99"),
		TakerBid:       decimal.RequireFromString("101"),
		TakerAsk:       decimal.RequireFromString("102"),
	})

	waitForTraderState(t, tr, StateWaitingFill)

	tr.mu.Lock()
	makerOrder := cloneTestOrder(tr.openFlow.makerOrder)
	makerQty := tr.openFlow.makerQty
	tr.mu.Unlock()
	if makerOrder == nil {
		t.Fatal("expected maker order to be tracked")
	}

	maker.emitFill(&exchanges.Fill{
		TradeID:       "maker-fill-1",
		OrderID:       makerOrder.OrderID,
		ClientOrderID: makerOrder.ClientOrderID,
		Symbol:        "BTC",
		Side:          makerOrder.Side,
		Price:         decimal.RequireFromString("99.91"),
		Quantity:      makerQty,
		Fee:           decimal.RequireFromString("0.01"),
		FeeAsset:      "USDC",
		Timestamp:     time.Now().UnixMilli(),
	})

	waitForCondition(t, time.Second, func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.position != nil
	}, "expected fill-only OrderFlow update to open position")

	taker.mu.Lock()
	gotOrders := append([]*exchanges.OrderParams(nil), taker.placedOrders...)
	taker.mu.Unlock()
	if len(gotOrders) != 1 {
		t.Fatalf("taker order count = %d, want 1 hedge order", len(gotOrders))
	}
	if !gotOrders[0].Quantity.Equal(makerQty) {
		t.Fatalf("hedge qty = %s, want %s", gotOrders[0].Quantity, makerQty)
	}

	if observed.FilterMessageSnippet("EVT hedge_done").Len() != 1 {
		t.Fatalf("hedge_done event logs = %d, want 1", observed.FilterMessageSnippet("EVT hedge_done").Len())
	}
}

func TestTrader_GracefulShutdownClosesOpenPosition(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()
	maker.autoEmitWS = true
	taker.autoEmitWS = true
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

func TestTraderLifecycleLogsRemainVisible(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)

	tr := newTestTraderWithPosition()
	maker := tr.maker.(*testExchange)
	taker := tr.taker.(*testExchange)
	maker.autoEmitWS = true
	taker.autoEmitWS = true
	tr.logger = zap.New(core).Sugar()

	tr.GracefulShutdown(2 * time.Second)

	if observed.FilterMessageSnippet("🛑 graceful shutdown:").Len() != 1 {
		t.Fatalf("graceful shutdown logs = %d, want 1", observed.FilterMessageSnippet("🛑 graceful shutdown:").Len())
	}
	if observed.FilterMessageSnippet("EVT close_done").Len() != 1 {
		t.Fatalf("close_done event logs = %d, want 1", observed.FilterMessageSnippet("EVT close_done").Len())
	}
	if observed.FilterMessageSnippet("🛑 graceful shutdown complete").Len() != 1 {
		t.Fatalf("graceful shutdown complete logs = %d, want 1", observed.FilterMessageSnippet("🛑 graceful shutdown complete").Len())
	}
}

func TestTrader_CloseUsesOrderFlowFillWithoutOrderUpdates(t *testing.T) {
	tr := newTestTraderWithPosition()
	maker, ok := tr.maker.(*testExchange)
	if !ok {
		t.Fatal("maker should be testExchange")
	}
	taker, ok := tr.taker.(*testExchange)
	if !ok {
		t.Fatal("taker should be testExchange")
	}
	maker.autoEmitWS = false
	taker.autoEmitWS = false

	done := make(chan struct{})
	go func() {
		tr.closePosition("fill-only-close")
		close(done)
	}()

	waitForCondition(t, time.Second, func() bool {
		maker.mu.Lock()
		makerCount := len(maker.placedOrders)
		maker.mu.Unlock()
		taker.mu.Lock()
		takerCount := len(taker.placedOrders)
		taker.mu.Unlock()
		return makerCount == 1 && takerCount == 1
	}, "expected close orders to be placed")

	maker.mu.Lock()
	makerParams := *maker.placedOrders[0]
	maker.mu.Unlock()
	taker.mu.Lock()
	takerParams := *taker.placedOrders[0]
	taker.mu.Unlock()

	now := time.Now().UnixMilli()
	maker.emitFill(&exchanges.Fill{
		TradeID:       "close-maker-fill-1",
		ClientOrderID: makerParams.ClientID,
		Symbol:        makerParams.Symbol,
		Side:          makerParams.Side,
		Price:         decimal.RequireFromString("100.5"),
		Quantity:      makerParams.Quantity,
		Timestamp:     now,
	})
	taker.emitFill(&exchanges.Fill{
		TradeID:       "close-taker-fill-1",
		ClientOrderID: takerParams.ClientID,
		Symbol:        takerParams.Symbol,
		Side:          takerParams.Side,
		Price:         decimal.RequireFromString("100.4"),
		Quantity:      takerParams.Quantity,
		Timestamp:     now,
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closePosition did not finish after fill-only OrderFlow updates")
	}

	if tr.position != nil {
		t.Fatal("position should be closed")
	}
	if tr.state != StateCooldown {
		t.Fatalf("state = %s, want cooldown", tr.state)
	}
}

func TestTrader_StartSubscribesTradingAccountOrders(t *testing.T) {
	tr, maker, taker := newActiveFlowTrader()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := tr.makerAccount.Start(ctx); err != nil {
		t.Fatalf("makerAccount.Start() error = %v", err)
	}
	if err := tr.takerAccount.Start(ctx); err != nil {
		t.Fatalf("takerAccount.Start() error = %v", err)
	}
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	update := &exchanges.Order{
		OrderID:       "maker-42",
		ClientOrderID: "cid-42",
		Status:        exchanges.OrderStatusPending,
	}
	maker.emitOrderUpdate(update)

	select {
	case got := <-tr.makerOrderCh:
		if got == nil || got.OrderID != update.OrderID {
			t.Fatalf("order update = %#v, want %s", got, update.OrderID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected trader to receive maker TradingAccount order update")
	}

	position := &exchanges.Position{
		Symbol:   tr.config.Symbol,
		Quantity: decimal.RequireFromString("0.003"),
	}
	taker.emitPositionUpdate(position)

	waitForCondition(t, 200*time.Millisecond, func() bool {
		snapshot, ok := tr.takerAccount.Position(tr.config.Symbol)
		return ok && snapshot.Quantity.Equal(position.Quantity)
	}, "expected taker TradingAccount position snapshot to update")
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

func TestTrader_BuildOpenTradeRecapIncludesTimingsAndBBO(t *testing.T) {
	tr := newTestTrader()
	now := time.Unix(1710000000, 0)
	tr.openFlow = &openFlowState{
		signal: &spread.Signal{
			Direction:      spread.LongMakerShortTaker,
			SpreadBps:      12.5,
			ZScore:         2.1,
			ExpectedProfit: 4.2,
			MakerBid:       decimal.RequireFromString("99.8"),
			MakerAsk:       decimal.RequireFromString("100.0"),
			TakerBid:       decimal.RequireFromString("100.6"),
			TakerAsk:       decimal.RequireFromString("100.8"),
			Timestamp:      now,
		},
		makerOrder: &exchanges.Order{
			OrderID: "maker-open",
			Price:   decimal.RequireFromString("99.99"),
		},
		lastHedgeOrder: &exchanges.Order{
			OrderID: "taker-hedge",
			Price:   decimal.RequireFromString("100.55"),
		},
		makerSide: exchanges.OrderSideBuy,
		takerSide: exchanges.OrderSideSell,
		makerQty:  decimal.RequireFromString("0.003"),

		signalTime:     now,
		makerPlacedAt:  now.Add(15 * time.Millisecond),
		fillDetectedAt: now.Add(240 * time.Millisecond),
		hedgeDoneAt:    now.Add(285 * time.Millisecond),

		signalSnapshot: &tradeBBO{
			CapturedAt: now,
			MakerBid:   decimal.RequireFromString("99.8"),
			MakerAsk:   decimal.RequireFromString("100.0"),
			TakerBid:   decimal.RequireFromString("100.6"),
			TakerAsk:   decimal.RequireFromString("100.8"),
			ZScore:     2.1,
			SpreadBps:  12.5,
		},
		makerPlacedSnapshot: &tradeBBO{
			CapturedAt: now.Add(15 * time.Millisecond),
			MakerBid:   decimal.RequireFromString("99.85"),
			MakerAsk:   decimal.RequireFromString("100.05"),
			TakerBid:   decimal.RequireFromString("100.62"),
			TakerAsk:   decimal.RequireFromString("100.82"),
		},
		fillSnapshot: &tradeBBO{
			CapturedAt: now.Add(240 * time.Millisecond),
			MakerBid:   decimal.RequireFromString("99.90"),
			MakerAsk:   decimal.RequireFromString("100.10"),
			TakerBid:   decimal.RequireFromString("100.64"),
			TakerAsk:   decimal.RequireFromString("100.84"),
		},
		hedgeSnapshot: &tradeBBO{
			CapturedAt: now.Add(285 * time.Millisecond),
			MakerBid:   decimal.RequireFromString("99.92"),
			MakerAsk:   decimal.RequireFromString("100.12"),
			TakerBid:   decimal.RequireFromString("100.58"),
			TakerAsk:   decimal.RequireFromString("100.78"),
		},
	}

	recap := tr.buildOpenTradeRecap(tr.openFlow, decimal.RequireFromString("0.003"))
	if recap == nil {
		t.Fatal("recap = nil, want data")
	}
	if recap.MakerOrderID != "maker-open" {
		t.Fatalf("maker order id = %q, want maker-open", recap.MakerOrderID)
	}
	if recap.TakerOrderID != "taker-hedge" {
		t.Fatalf("taker order id = %q, want taker-hedge", recap.TakerOrderID)
	}
	if recap.Timings.SignalToMaker != 15*time.Millisecond {
		t.Fatalf("signal->maker = %s, want 15ms", recap.Timings.SignalToMaker)
	}
	if recap.Timings.MakerToFill != 225*time.Millisecond {
		t.Fatalf("maker->fill = %s, want 225ms", recap.Timings.MakerToFill)
	}
	if recap.Timings.FillToHedge != 45*time.Millisecond {
		t.Fatalf("fill->hedge = %s, want 45ms", recap.Timings.FillToHedge)
	}
	if recap.Timings.Total != 285*time.Millisecond {
		t.Fatalf("total = %s, want 285ms", recap.Timings.Total)
	}
	if recap.Signal == nil || !recap.Signal.MakerAsk.Equal(decimal.RequireFromString("100.0")) {
		t.Fatalf("signal snapshot = %#v, want maker ask 100.0", recap.Signal)
	}
	if recap.HedgeDone == nil || !recap.HedgeDone.TakerBid.Equal(decimal.RequireFromString("100.58")) {
		t.Fatalf("hedge snapshot = %#v, want taker bid 100.58", recap.HedgeDone)
	}
}

func TestTrader_FinalizeOpenFlowLogsStructuredRecap(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	tr, _, _ := newActiveFlowTrader()
	tr.logger = zap.New(core).Sugar()

	now := time.Unix(1710000100, 0)
	tr.roundID = 7
	tr.openFlow.signal = &spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      14.2,
		ZScore:         2.4,
		ExpectedProfit: 5.1,
	}
	tr.openFlow.lastHedgeOrder = &exchanges.Order{
		OrderID: "taker-7",
		Price:   decimal.RequireFromString("100.8"),
	}
	tr.openFlow.signalTime = now
	tr.openFlow.makerPlacedAt = now.Add(10 * time.Millisecond)
	tr.openFlow.fillDetectedAt = now.Add(160 * time.Millisecond)
	tr.openFlow.hedgeDoneAt = now.Add(215 * time.Millisecond)
	tr.openFlow.signalSnapshot = &tradeBBO{
		CapturedAt: now,
		MakerBid:   decimal.RequireFromString("99.7"),
		MakerAsk:   decimal.RequireFromString("99.9"),
		TakerBid:   decimal.RequireFromString("100.5"),
		TakerAsk:   decimal.RequireFromString("100.7"),
	}
	tr.openFlow.makerPlacedSnapshot = &tradeBBO{
		CapturedAt: now.Add(10 * time.Millisecond),
		MakerBid:   decimal.RequireFromString("99.72"),
		MakerAsk:   decimal.RequireFromString("99.92"),
		TakerBid:   decimal.RequireFromString("100.48"),
		TakerAsk:   decimal.RequireFromString("100.68"),
	}
	tr.openFlow.fillSnapshot = &tradeBBO{
		CapturedAt: now.Add(160 * time.Millisecond),
		MakerBid:   decimal.RequireFromString("99.74"),
		MakerAsk:   decimal.RequireFromString("99.94"),
		TakerBid:   decimal.RequireFromString("100.46"),
		TakerAsk:   decimal.RequireFromString("100.66"),
	}
	tr.openFlow.hedgeSnapshot = &tradeBBO{
		CapturedAt: now.Add(215 * time.Millisecond),
		MakerBid:   decimal.RequireFromString("99.76"),
		MakerAsk:   decimal.RequireFromString("99.96"),
		TakerBid:   decimal.RequireFromString("100.44"),
		TakerAsk:   decimal.RequireFromString("100.64"),
	}

	tr.finalizeOpenFlow(&exchanges.Order{
		OrderID: "maker-7",
		Price:   decimal.RequireFromString("99.91"),
	}, decimal.RequireFromString("0.003"))

	entries := observed.FilterMessageSnippet("📘 recap").All()
	if len(entries) != 1 {
		t.Fatalf("recap logs = %d, want 1", len(entries))
	}
	msg := entries[0].Message
	for _, want := range []string{
		"maker=10ms",
		"fill=150ms",
		"hedge=55ms",
		"total=215ms",
		"signal_bbo",
		"maker_bbo",
		"fill_bbo",
		"hedge_bbo",
		"maker-7",
		"taker-7",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("recap log missing %q: %s", want, msg)
		}
	}
}

func TestBuildLatencyReportUsesExchangeAndLocalTimestamps(t *testing.T) {
	submitLocal := time.Unix(1710000000, 0)
	ackLocal := submitLocal.Add(180 * time.Millisecond)
	firstFillLocal := submitLocal.Add(430 * time.Millisecond)

	report := buildLatencyReport(
		submitLocal,
		ackLocal,
		&exchanges.Order{Timestamp: submitLocal.Add(120 * time.Millisecond).UnixMilli()},
		firstFillLocal,
		&exchanges.Fill{Timestamp: submitLocal.Add(350 * time.Millisecond).UnixMilli()},
	)

	if report.SubmitToExchange != 120*time.Millisecond {
		t.Fatalf("SubmitToExchange = %s, want 120ms", report.SubmitToExchange)
	}
	if report.ExchangeToLocalAck != 60*time.Millisecond {
		t.Fatalf("ExchangeToLocalAck = %s, want 60ms", report.ExchangeToLocalAck)
	}
	if report.ExchangeToLocalFill != 80*time.Millisecond {
		t.Fatalf("ExchangeToLocalFill = %s, want 80ms", report.ExchangeToLocalFill)
	}
	if report.SubmitToFirstFillExchange != 350*time.Millisecond {
		t.Fatalf("SubmitToFirstFillExchange = %s, want 350ms", report.SubmitToFirstFillExchange)
	}
	if report.SubmitToFirstFillLocal != 430*time.Millisecond {
		t.Fatalf("SubmitToFirstFillLocal = %s, want 430ms", report.SubmitToFirstFillLocal)
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
	}, nil, tr.makerOrderCh, tr.takerOrderCh, true)
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
	}, nil, tr.makerOrderCh, tr.takerOrderCh, true)
	if err == nil {
		t.Fatal("verifyCloseLeg error = nil, want error")
	}
	if ok {
		t.Fatal("verifyCloseLeg = true, want false")
	}
}

func TestTraderHandleSignalRecordsRoundEvents(t *testing.T) {
	tr := newTestTrader()
	tr.makerAccount = nil

	eventsFile, err := os.Create(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("Create(events.jsonl): %v", err)
	}
	sink := runlog.NewEventSink(eventsFile)
	t.Cleanup(func() { _ = sink.Close() })
	tr.SetEventSink(sink)

	tr.HandleSignal(&spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.5,
		ZScore:         2.1,
		ExpectedProfit: 4.2,
		Quantity:       decimal.RequireFromString("0.002"),
		MakerAsk:       decimal.RequireFromString("100"),
		MakerBid:       decimal.RequireFromString("99.9"),
	})

	waitForCondition(t, 500*time.Millisecond, func() bool {
		data, readErr := os.ReadFile(eventsFile.Name())
		return readErr == nil && strings.Count(string(data), `"category":"round"`) >= 2
	}, "round events to be recorded")

	data, err := os.ReadFile(eventsFile.Name())
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", eventsFile.Name(), err)
	}
	if !strings.Contains(string(data), `"type":"signal"`) {
		t.Fatalf("missing signal event: %s", data)
	}
	if !strings.Contains(string(data), `"type":"manual_intervention"`) {
		t.Fatalf("missing manual_intervention event: %s", data)
	}
}
