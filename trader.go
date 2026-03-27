package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// ArbPosition represents an open arbitrage position.
type ArbPosition struct {
	Direction          SpreadDirection
	OpenSpread         float64 // spread BPS at open
	OpenZScore         float64 // Z-Score at open
	OpenExpectedProfit float64 // modeled net profit BPS at open
	OpenTime           time.Time
	OpenQuantity       decimal.Decimal

	// Order details (nil in dry-run)
	LongOrder  *exchanges.Order
	ShortOrder *exchanges.Order

	// Which exchange has the long vs short
	LongExchange  string
	ShortExchange string
}

// Trader executes arbitrage trades based on signals from SpreadEngine.
type Trader struct {
	maker  exchanges.Exchange
	taker  exchanges.Exchange
	engine *SpreadEngine
	config *Config
	logger *zap.SugaredLogger

	mu              sync.Mutex
	position        *ArbPosition // nil = no open position
	lastTrade       time.Time    // for cooldown enforcement
	lastSignalTime  time.Time    // for signal acceptance cooldown
	lastLogTime     time.Time    // for stable position log interval
	state           ExecutionState
	completedRounds int
	roundID         int // auto-incrementing round identifier for log tracing
	openFlow        *openFlowState

	// Parent context from Start(); used for shutdown-aware operations.
	ctx context.Context

	// PnL tracking (optional, set via SetPnLTracker).
	pnl *PnLTracker

	// For maker-taker mode: order update channel
	makerOrderCh chan *exchanges.Order
	takerOrderCh chan *exchanges.Order
}

var closeLegVerifyTimeout = 5 * time.Second

func (t *Trader) roundTag() string {
	return fmt.Sprintf("R%03d", t.roundID)
}

// NewTrader creates a new trader.
func NewTrader(maker, taker exchanges.Exchange, engine *SpreadEngine, cfg *Config, logger *zap.SugaredLogger) *Trader {
	return &Trader{
		maker:        maker,
		taker:        taker,
		engine:       engine,
		config:       cfg,
		logger:       logger,
		makerOrderCh: make(chan *exchanges.Order, 100),
		takerOrderCh: make(chan *exchanges.Order, 100),
		state:        StateIdle,
	}
}

// SetPnLTracker attaches a PnL tracker to the trader.
func (t *Trader) SetPnLTracker(p *PnLTracker) {
	t.pnl = p
}

// Start begins the trader's monitoring loops.
func (t *Trader) Start(ctx context.Context) error {
	t.ctx = ctx

	// Subscribe to order updates for maker-taker fill tracking
	if !t.config.DryRun {
		watchErrCh := make(chan error, 2)
		startWatch := func(name string, ex exchanges.Exchange, out chan *exchanges.Order) {
			go func() {
				err := ex.WatchOrders(ctx, func(o *exchanges.Order) {
					select {
					case out <- o:
					case <-ctx.Done():
					}
				})
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					// Some adapters register the subscription asynchronously and
					// return nil immediately instead of blocking for stream lifetime.
					// Treat that as a successful startup rather than a fatal error.
					t.logger.Debugf("%s WatchOrders registered in async mode", name)
					return
				}
				err = fmt.Errorf("%s WatchOrders: %w", name, err)
				select {
				case watchErrCh <- err:
				default:
				}
			}()
		}

		startWatch(t.config.MakerExchange, t.maker, t.makerOrderCh)
		startWatch(t.config.TakerExchange, t.taker, t.takerOrderCh)

		startupTimer := time.NewTimer(200 * time.Millisecond)
		defer startupTimer.Stop()
		select {
		case err := <-watchErrCh:
			return err
		case <-startupTimer.C:
		case <-ctx.Done():
			return ctx.Err()
		}

		go t.monitorWatchOrderErrors(ctx, watchErrCh)
	}

	// Position monitor loop (check close conditions periodically)
	go t.monitorLoop(ctx)
	return nil
}

func (t *Trader) monitorWatchOrderErrors(ctx context.Context, errCh <-chan error) {
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if err == nil {
				continue
			}
			t.logger.Errorf("❌ order stream failed: %v", err)
			go telegram.Notify(fmt.Sprintf("🚨 Order stream failed — manual intervention required\nErr: %v", err))
			t.mu.Lock()
			t.state = StateManualIntervention
			t.mu.Unlock()
			return
		}
	}
}

type openFlowState struct {
	signal         *SpreadSignal
	makerOrder     *exchanges.Order
	makerSide      exchanges.OrderSide
	takerSide      exchanges.OrderSide
	makerQty       decimal.Decimal
	hedgedQty      decimal.Decimal
	lastHedgeOrder *exchanges.Order

	// Timing
	signalTime     time.Time // when signal was accepted
	makerPlacedAt  time.Time // when PlaceOrder returned
	fillDetectedAt time.Time // when WS/REST detected fill
	hedgeDoneAt    time.Time // when hedge completed
}

// HandleSignal processes a signal from the SpreadEngine.
func (t *Trader) HandleSignal(sig *SpreadSignal) {
	if sig == nil {
		return
	}

	t.mu.Lock()
	now := time.Now()
	if !t.canStartNextRoundLocked(now) {
		t.mu.Unlock()
		return
	}

	// Prevent rapid-fire signals after PostOnly rejections.
	if !t.lastSignalTime.IsZero() && now.Sub(t.lastSignalTime) < 3*time.Second {
		t.mu.Unlock()
		return
	}
	t.lastSignalTime = now
	t.roundID++
	roundTag := fmt.Sprintf("R%03d", t.roundID)

	t.logger.Infof("%s 🟢 signal  %s  spread=%.1fbps Z=%.2f profit=%.1fbps",
		roundTag, sig.Direction, sig.SpreadBps, sig.ZScore, sig.ExpectedProfit)

	if t.config.DryRun {
		t.logger.Infof("%s 🔸 [DRY] open %s qty=%s", roundTag, sig.Direction, t.config.Quantity)
		t.position = &ArbPosition{
			Direction:          sig.Direction,
			OpenSpread:         sig.SpreadBps,
			OpenZScore:         sig.ZScore,
			OpenExpectedProfit: sig.ExpectedProfit,
			OpenTime:           now,
			OpenQuantity:       t.config.Quantity,
		}
		t.lastTrade = now
		t.state = StatePositionOpen
		t.mu.Unlock()
		return
	}

	t.state = StatePlacingMaker
	sigCopy := *sig
	signalTime := now
	t.mu.Unlock()

	// Execute maker-taker asynchronously so HandleSignal only performs the idle -> placing_maker transition.
	go t.openMakerTaker(&sigCopy, signalTime)
}

// openMakerTaker places a maker order on maker exchange, then hedges via taker on fill.
func (t *Trader) openMakerTaker(sig *SpreadSignal, signalTime time.Time) {
	if sig == nil {
		return
	}

	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	qty := t.config.Quantity

	// Determine sides
	var makerSide, takerSide exchanges.OrderSide
	var makerPrice decimal.Decimal

	if sig.Direction == LongMakerShortTaker {
		makerSide = exchanges.OrderSideBuy
		takerSide = exchanges.OrderSideSell
		makerPrice = sig.MakerAsk // buy at ask
	} else {
		makerSide = exchanges.OrderSideSell
		takerSide = exchanges.OrderSideBuy
		makerPrice = sig.MakerBid // sell at bid
	}

	// Apply 1 bps offset to stay on maker side and reduce PostOnly rejections
	offset := makerPrice.Mul(decimal.NewFromFloat(0.0001)) // 1 bps
	if makerSide == exchanges.OrderSideBuy {
		makerPrice = makerPrice.Sub(offset) // lower buy price
	} else {
		makerPrice = makerPrice.Add(offset) // raise sell price
	}

	// Fetch precision for maker order
	details, err := t.maker.FetchSymbolDetails(ctx, t.config.Symbol)
	if err != nil {
		t.logger.Errorw("failed to fetch symbol details", "err", err)
		t.resetOpenFlow(StateIdle)
		return
	}
	makerPrice = exchanges.RoundToPrecision(makerPrice, details.PricePrecision)
	qty = exchanges.FloorToPrecision(qty, details.QuantityPrecision)

	// Place maker (POST_ONLY) order
	cid := exchanges.GenerateID()
	makerOrder, err := t.maker.PlaceOrder(ctx, &exchanges.OrderParams{
		Symbol:      t.config.Symbol,
		Side:        makerSide,
		Type:        exchanges.OrderTypePostOnly,
		Price:       makerPrice,
		Quantity:    qty,
		TimeInForce: exchanges.TimeInForcePO,
		ClientID:    cid,
	})
	if err != nil {
		t.logger.Errorf("%s ❌ maker failed: %v", t.roundTag(), err)
		go telegram.Notify(fmt.Sprintf("❌ Maker order failed: %v", err))
		t.resetOpenFlow(StateIdle)
		return
	}
	makerPlacedAt := time.Now()

	t.mu.Lock()
	t.openFlow = &openFlowState{
		signal:        sig,
		makerOrder:    makerOrder,
		makerSide:     makerSide,
		takerSide:     takerSide,
		makerQty:      qty,
		signalTime:    signalTime,
		makerPlacedAt: makerPlacedAt,
	}
	t.state = StateWaitingFill
	t.mu.Unlock()

	t.logger.Infof("%s 📌 maker %s %s %s qty=%s cid=%s (%dms)",
		t.roundTag(), makerSide, t.config.MakerExchange, makerPrice, qty, cid,
		makerPlacedAt.Sub(signalTime).Milliseconds())

	// Wait for maker fill with timeout.
	timeout := time.NewTimer(t.config.MakerTimeout)
	defer timeout.Stop()

	for {
		select {
		case update := <-t.makerOrderCh:
			if done, err := t.handleMakerOrderUpdate(ctx, update, makerOrder, cid, qty); err != nil {
				t.logger.Errorf("%s maker update error: %v", t.roundTag(), err)
				return
			} else if done {
				return
			}
		case <-timeout.C:
			if err := t.handleMakerTimeout(ctx); err != nil {
				t.logger.Warnf("%s timeout handling failed: %v", t.roundTag(), err)
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

func (t *Trader) handleMakerFillForTest(filledQty decimal.Decimal) error {
	return t.hedgeMakerQty(context.Background(), filledQty)
}

func (t *Trader) handleMakerTimeoutForTest() {
	_ = t.handleMakerTimeout(context.Background())
}

func (t *Trader) hedgeMakerQty(ctx context.Context, filledQty decimal.Decimal) error {
	t.mu.Lock()
	flow := t.openFlow
	t.mu.Unlock()
	if flow == nil || flow.makerOrder == nil {
		return nil
	}

	update := &exchanges.Order{
		OrderID:        flow.makerOrder.OrderID,
		ClientOrderID:  flow.makerOrder.ClientOrderID,
		Status:         exchanges.OrderStatusFilled,
		FilledQuantity: filledQty,
	}
	_, err := t.handleMakerOrderUpdate(ctx, update, flow.makerOrder, "", flow.makerQty)
	return err
}

func (t *Trader) handleMakerTimeout(ctx context.Context) error {
	t.mu.Lock()
	flow := t.openFlow
	if flow == nil {
		t.state = StateIdle
		t.mu.Unlock()
		return nil
	}
	t.state = StateClosing
	t.mu.Unlock()

	t.logger.Infof("%s ⏰ timeout, cancelling order %s", t.roundTag(), flow.makerOrder.OrderID)

	// Step 1: Cancel the maker order.
	if flow.makerOrder != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("%s cancel failed: %v", t.roundTag(), err)
		}
		cancel()
	}

	// Step 2: Wait up to 3s for WatchOrders channel to deliver terminal status.
	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	settled, _ := t.settleMakerFlow(waitCtx, flow)
	cancel()
	if settled {
		return nil
	}

	// Step 3: Channel didn't deliver — REST fallback to fetch order status.
	t.logger.Infof("%s 🔄 REST fallback for %s", t.roundTag(), flow.makerOrder.OrderID)

	filledQty := decimal.Zero
	var orderStatus exchanges.OrderStatus
	if flow.makerOrder != nil {
		restCtx, restCancel := context.WithTimeout(context.Background(), 5*time.Second)
		order, err := t.maker.FetchOrderByID(restCtx, flow.makerOrder.OrderID, t.config.Symbol)
		restCancel()
		if err != nil {
			t.logger.Errorf("%s ❌ REST fallback failed, order status UNKNOWN: %v", t.roundTag(), err)
			go telegram.Notify(fmt.Sprintf("🚨 Maker order status UNKNOWN after timeout — manual intervention required\nOrderID: %s\nErr: %v",
				flow.makerOrder.OrderID, err))
			t.mu.Lock()
			t.state = StateManualIntervention
			t.mu.Unlock()
			return err
		}
		if order != nil {
			mergeOrderDetails(flow.makerOrder, order)
			filledQty = order.FilledQuantity
			orderStatus = order.Status
			t.logger.Infof("%s REST status=%s filled=%s", t.roundTag(), order.Status, filledQty)
		}
	}

	// Step 4: Handle partial fills - cancel remaining if still open
	if filledQty.GreaterThan(decimal.Zero) && filledQty.LessThan(flow.makerQty) &&
		orderStatus == exchanges.OrderStatusPartiallyFilled {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("%s partial fill cancel failed: %v", t.roundTag(), err)
		}
		cancel()
	}

	// Step 5: If any quantity was filled, hedge it; otherwise go idle.
	if filledQty.GreaterThan(decimal.Zero) {
		hedgeOrder, err := t.hedgeMakerDelta(ctx, flow.makerOrder, filledQty)
		if err != nil {
			return err
		}
		if hedgeOrder != nil {
			t.mu.Lock()
			if t.openFlow != nil {
				t.openFlow.lastHedgeOrder = hedgeOrder
			}
			t.mu.Unlock()
		}
		t.finalizeOpenFlow(flow.makerOrder, filledQty)
	} else {
		t.logger.Infof("%s no fills after timeout, idle", t.roundTag())
		t.resetOpenFlow(StateIdle)
	}

	return nil
}

func (t *Trader) matchesOpenMakerOrder(update *exchanges.Order, makerOrder *exchanges.Order, clientID string) bool {
	if update == nil {
		return false
	}
	if makerOrder != nil {
		if update.OrderID == makerOrder.OrderID {
			return true
		}
		if makerOrder.ClientOrderID != "" && update.ClientOrderID == makerOrder.ClientOrderID {
			return true
		}
	}
	if clientID != "" && update.ClientOrderID == clientID {
		return true
	}
	return false
}

func (t *Trader) settleMakerFlow(ctx context.Context, flow *openFlowState) (bool, error) {
	for {
		select {
		case update := <-t.makerOrderCh:
			done, err := t.handleMakerOrderUpdate(ctx, update, flow.makerOrder, "", flow.makerQty)
			if err != nil {
				return false, err
			}
			if done {
				return true, nil
			}
		case <-ctx.Done():
			t.mu.Lock()
			if t.openFlow != nil {
				t.state = StateClosing
			}
			t.mu.Unlock()
			return false, ctx.Err()
		}
	}
}

func (t *Trader) handleMakerOrderUpdate(ctx context.Context, update *exchanges.Order, makerOrder *exchanges.Order, clientID string, fallbackQty decimal.Decimal) (bool, error) {
	if !t.matchesOpenMakerOrder(update, makerOrder, clientID) {
		return false, nil
	}
	mergeOrderDetails(makerOrder, update)

	flowQty := fallbackQty
	t.mu.Lock()
	flow := t.openFlow
	if flow != nil && flow.makerOrder != nil && makerOrder != nil && flow.makerOrder.OrderID == makerOrder.OrderID {
		flowQty = flow.makerQty
	}
	// Record fill detection time
	if flow != nil && flow.fillDetectedAt.IsZero() {
		flow.fillDetectedAt = time.Now()
	}
	t.mu.Unlock()

	targetQty := update.FilledQuantity
	if targetQty.IsZero() && update.Status == exchanges.OrderStatusFilled {
		targetQty = flowQty
	}

	if targetQty.GreaterThan(decimal.Zero) {
		hedgeStart := time.Now()
		hedgeOrder, err := t.hedgeMakerDelta(ctx, makerOrder, targetQty)
		if err != nil {
			return true, err
		}
		if hedgeOrder != nil {
			hedgeDone := time.Now()
			t.mu.Lock()
			if t.openFlow != nil {
				t.openFlow.lastHedgeOrder = hedgeOrder
				t.openFlow.hedgeDoneAt = hedgeDone
			}
			t.mu.Unlock()
			t.logger.Infof("%s 🚨 hedge done (%dms)", t.roundTag(), hedgeDone.Sub(hedgeStart).Milliseconds())
		}
	}

	switch update.Status {
	case exchanges.OrderStatusPartiallyFilled:
		t.mu.Lock()
		if t.openFlow != nil {
			t.state = StateWaitingFill
		}
		t.mu.Unlock()
		return false, nil
	case exchanges.OrderStatusFilled, exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
		if targetQty.GreaterThan(decimal.Zero) {
			t.finalizeOpenFlow(makerOrder, targetQty)
		} else {
			t.logger.Infof("%s ⚠️ maker %s (no fills), idle", t.roundTag(), update.Status)
			t.resetOpenFlow(StateIdle)
		}
		return true, nil
	default:
		return false, nil
	}
}

func (t *Trader) hedgeMakerDelta(ctx context.Context, makerOrder *exchanges.Order, targetQty decimal.Decimal) (*exchanges.Order, error) {
	t.mu.Lock()
	flow := t.openFlow
	if flow == nil {
		t.mu.Unlock()
		return nil, nil
	}
	delta := targetQty.Sub(flow.hedgedQty)
	if delta.LessThanOrEqual(decimal.Zero) {
		t.mu.Unlock()
		return nil, nil
	}
	t.mu.Unlock()

	slippage := decimal.NewFromFloat(t.config.Slippage)
	hedgeOrder, err := exchanges.PlaceMarketOrderWithSlippage(
		ctx, t.taker, t.config.Symbol,
		flow.takerSide, delta, slippage,
	)
	if err != nil {
		t.logger.Errorf("%s ❌ hedge failed — UNHEDGED qty=%s: %v", t.roundTag(), delta, err)
		go telegram.Notify(fmt.Sprintf("🚨 HEDGE FAILED — UNHEDGED EXPOSURE!\nQty: %s\nErr: %v", delta, err))
		t.mu.Lock()
		t.state = StateManualIntervention
		t.mu.Unlock()
		return nil, err
	}

	// Verify taker order fill via WatchOrders channel.
	// PlaceOrder returns Pending — we must confirm actual fill status.
	if confirmed, verifyErr := t.waitTakerFill(ctx, hedgeOrder, delta); !confirmed {
		errMsg := fmt.Sprintf("taker order rejected/unfilled: %v", verifyErr)
		t.logger.Errorf("%s ❌ hedge REJECTED — UNHEDGED qty=%s: %s", t.roundTag(), delta, errMsg)
		go telegram.Notify(fmt.Sprintf("🚨 HEDGE REJECTED — UNHEDGED!\nQty: %s\n%s", delta, errMsg))
		t.mu.Lock()
		t.state = StateManualIntervention
		t.mu.Unlock()
		return nil, fmt.Errorf("%s", errMsg)
	}

	t.mu.Lock()
	if t.openFlow != nil && targetQty.GreaterThan(t.openFlow.hedgedQty) {
		t.openFlow.hedgedQty = targetQty
	}
	t.mu.Unlock()

	if targetQty.LessThan(flow.makerQty) && makerOrder != nil {
		// Cancel remaining maker order after partial fill
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("%s partial cancel failed: %v", t.roundTag(), err)
			// Don't block on cancel failure - monitor via WatchOrders
		}
		cancel()
	}

	t.mu.Lock()
	if t.openFlow != nil && targetQty.GreaterThanOrEqual(t.openFlow.hedgedQty) && targetQty.LessThan(t.openFlow.makerQty) {
		t.state = StateWaitingFill
	}
	t.mu.Unlock()

	return hedgeOrder, nil
}

// waitTakerFill waits for taker order confirmation via WatchOrders.
// Returns (true, nil) if filled, (false, err) if rejected/cancelled/timeout.
func (t *Trader) waitTakerFill(ctx context.Context, order *exchanges.Order, expectedDelta decimal.Decimal) (bool, error) {
	if order == nil {
		return false, fmt.Errorf("nil order")
	}

	timeout := time.After(10 * time.Second)
	for {
		select {
		case update := <-t.takerOrderCh:
			if update == nil {
				continue
			}
			// Match by OrderID or ClientOrderID
			if update.OrderID != order.OrderID && update.ClientOrderID != order.ClientOrderID {
				continue
			}
			mergeOrderDetails(order, update)
			switch update.Status {
			case exchanges.OrderStatusFilled:
				t.logger.Infof("%s ✅ taker confirmed FILLED", t.roundTag())
				return true, nil
			case exchanges.OrderStatusPartiallyFilled:
				// Keep waiting for full fill or terminal status
				continue
			case exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
				return false, fmt.Errorf("status=%s", update.Status)
			default:
				continue
			}
		case <-timeout:
			// No WS event in 10s — verify via FetchPositions as fallback
			t.logger.Warnf("%s taker fill timeout, checking positions...", t.roundTag())
			confirmed, err := t.verifyTakerPosition(ctx, expectedDelta)
			if confirmed {
				if snapshot, snapshotErr := t.fetchOrderSnapshot(ctx, t.taker, order); snapshotErr == nil {
					mergeOrderDetails(order, snapshot)
				}
			}
			return confirmed, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// verifyTakerPosition checks if taker exchange has the expected position after this hedge.
// expectedDelta is the quantity we just tried to hedge (not cumulative).
func (t *Trader) verifyTakerPosition(ctx context.Context, expectedDelta decimal.Decimal) (bool, error) {
	perp, ok := t.taker.(exchanges.PerpExchange)
	if !ok {
		// Can't verify — assume success
		return true, nil
	}

	t.mu.Lock()
	flow := t.openFlow
	// Total expected position = previously hedged + this delta
	totalExpected := expectedDelta
	if flow != nil {
		totalExpected = flow.hedgedQty.Add(expectedDelta)
	}
	t.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	positions, err := perp.FetchPositions(checkCtx)
	if err != nil {
		return false, fmt.Errorf("fetch positions failed: %w", err)
	}
	for _, p := range positions {
		if strings.EqualFold(p.Symbol, t.config.Symbol) {
			absQty := p.Quantity.Abs()
			if absQty.GreaterThanOrEqual(totalExpected) {
				t.logger.Infof("%s ✅ taker position verified: %s %s (expected >=%s)",
					t.roundTag(), p.Side, absQty, totalExpected)
				return true, nil
			}
			// Position exists but too small
			return false, fmt.Errorf("taker position %s < expected %s", absQty, totalExpected)
		}
	}
	return false, fmt.Errorf("no taker position found after hedge")
}

func (t *Trader) finalizeOpenFlow(makerOrder *exchanges.Order, filledQty decimal.Decimal) {
	t.mu.Lock()
	flow := t.openFlow
	if flow == nil {
		t.mu.Unlock()
		return
	}

	longName, shortName := t.config.MakerExchange, t.config.TakerExchange
	longOrder, shortOrder := makerOrder, flow.lastHedgeOrder
	if flow.signal.Direction == LongTakerShortMaker {
		longName, shortName = shortName, longName
		longOrder, shortOrder = flow.lastHedgeOrder, makerOrder
	}

	t.position = &ArbPosition{
		Direction:          flow.signal.Direction,
		OpenSpread:         flow.signal.SpreadBps,
		OpenZScore:         flow.signal.ZScore,
		OpenExpectedProfit: flow.signal.ExpectedProfit,
		OpenTime:           time.Now(),
		OpenQuantity:       filledQty,
		LongOrder:          longOrder,
		ShortOrder:         shortOrder,
		LongExchange:       longName,
		ShortExchange:      shortName,
	}
	t.lastTrade = time.Now()
	t.state = StatePositionOpen
	t.openFlow = nil
	t.mu.Unlock()

	t.logger.Infof("%s ✅ opened  long=%s short=%s spread=%.1fbps qty=%s",
		t.roundTag(), longName, shortName, flow.signal.SpreadBps, filledQty)

	// Timing summary: signal → maker → fill → hedge → done
	var timingParts []string
	if !flow.signalTime.IsZero() && !flow.makerPlacedAt.IsZero() {
		timingParts = append(timingParts, fmt.Sprintf("maker=%dms", flow.makerPlacedAt.Sub(flow.signalTime).Milliseconds()))
	}
	if !flow.makerPlacedAt.IsZero() && !flow.fillDetectedAt.IsZero() {
		timingParts = append(timingParts, fmt.Sprintf("fill=%dms", flow.fillDetectedAt.Sub(flow.makerPlacedAt).Milliseconds()))
	}
	if !flow.fillDetectedAt.IsZero() && !flow.hedgeDoneAt.IsZero() {
		timingParts = append(timingParts, fmt.Sprintf("hedge=%dms", flow.hedgeDoneAt.Sub(flow.fillDetectedAt).Milliseconds()))
	}
	if !flow.signalTime.IsZero() {
		timingParts = append(timingParts, fmt.Sprintf("total=%dms", time.Since(flow.signalTime).Milliseconds()))
	}
	if len(timingParts) > 0 {
		t.logger.Infof("%s ⏱ %s", t.roundTag(), strings.Join(timingParts, " "))
	}

	go telegram.Notify(fmt.Sprintf("✅ %s Opened\nLong %s / Short %s\nSpread: %.1fbps | Z: %.2f | Qty: %s",
		t.roundTag(), longName, shortName, flow.signal.SpreadBps, flow.signal.ZScore, filledQty))
}

func (t *Trader) resetOpenFlow(state ExecutionState) {
	t.mu.Lock()
	t.openFlow = nil
	t.state = state
	t.mu.Unlock()
}

// monitorLoop periodically checks close conditions for open positions.
func (t *Trader) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.checkCloseConditions()
			t.checkLoopControls()
			if t.pnl != nil {
				t.pnl.PeriodicRefresh(ctx)
			}
		}
	}
}

func (t *Trader) checkLoopControls() {
	t.mu.Lock()
	t.releaseCooldownIfExpiredLocked(time.Now())
	t.mu.Unlock()
}

// checkCloseConditions evaluates whether to close the current position.
func (t *Trader) checkCloseConditions() {
	t.mu.Lock()
	pos := t.position
	if pos == nil || t.state != StatePositionOpen {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	zAB, zBA := t.engine.GetCurrentZ()

	// Get the Z-Score for the position's direction
	var currentZ float64
	if pos.Direction == LongMakerShortTaker {
		currentZ = zAB
	} else {
		currentZ = zBA
	}

	holdTime := time.Since(pos.OpenTime)

	// Check close conditions
	reason := ""

	// 1. Spread reverted — take profit
	if currentZ < t.config.ZClose {
		reason = fmt.Sprintf("spread reverted (Z=%.2f < %.2f)", currentZ, t.config.ZClose)
	}

	// 2. Spread reversed — stop loss
	if currentZ < t.config.ZStop {
		reason = fmt.Sprintf("stop loss (Z=%.2f < %.2f)", currentZ, t.config.ZStop)
	}

	// 3. Max hold time exceeded
	if holdTime > t.config.MaxHoldTime {
		reason = fmt.Sprintf("max hold time exceeded (%s > %s)", holdTime.Round(time.Second), t.config.MaxHoldTime)
	}

	if reason == "" {
		// Log status periodically (every 10 seconds, using timestamp for stable interval)
		now := time.Now()
		if t.lastLogTime.IsZero() || now.Sub(t.lastLogTime) >= 10*time.Second {
			t.logger.Infof("%s 📊 hold  Z=%.2f (open=%.1fbps) %s",
				t.roundTag(), currentZ, pos.OpenSpread, holdTime.Round(time.Second))
			t.lastLogTime = now
		}
		return
	}

	t.closePosition(reason)
}

// closePosition closes the current position.
func (t *Trader) closePosition(reason string) {
	t.mu.Lock()
	pos := t.position
	if pos == nil || t.state != StatePositionOpen {
		t.mu.Unlock()
		return
	}
	t.state = StateClosing
	t.mu.Unlock()

	t.logger.Infof("%s 🔴 closing  %s  held=%s",
		t.roundTag(), reason, time.Since(pos.OpenTime).Round(time.Second))

	closeStart := time.Now()

	if t.config.DryRun {
		t.logger.Infof("%s 🔸 [DRY] close %s", t.roundTag(), pos.Direction)
		t.finishSuccessfulClose()
		return
	}

	closeCtx := t.ctx
	if closeCtx == nil || closeCtx.Err() != nil {
		closeCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(closeCtx, 30*time.Second)
	defer cancel()

	qty := t.config.Quantity
	if pos.OpenQuantity.GreaterThan(decimal.Zero) {
		qty = pos.OpenQuantity
	}
	slippage := decimal.NewFromFloat(t.config.Slippage)

	var longExchange, shortExchange exchanges.Exchange
	if pos.Direction == LongMakerShortTaker {
		longExchange, shortExchange = t.maker, t.taker
	} else {
		longExchange, shortExchange = t.taker, t.maker
	}

	// Close both sides concurrently with ReduceOnly to prevent accidental
	// position reversal if recorded qty exceeds actual remaining position.
	var wg sync.WaitGroup
	wg.Add(2)
	type closeLegResult struct {
		leg   string
		order *exchanges.Order
		err   error
	}
	errCh := make(chan closeLegResult, 2)

	go func() {
		defer wg.Done()
		order, err := longExchange.PlaceOrder(ctx, &exchanges.OrderParams{
			Symbol:     t.config.Symbol,
			Side:       exchanges.OrderSideSell,
			Type:       exchanges.OrderTypeMarket,
			Quantity:   qty,
			Slippage:   slippage,
			ReduceOnly: true,
		})
		errCh <- closeLegResult{leg: "long", order: order, err: err}
	}()

	go func() {
		defer wg.Done()
		order, err := shortExchange.PlaceOrder(ctx, &exchanges.OrderParams{
			Symbol:     t.config.Symbol,
			Side:       exchanges.OrderSideBuy,
			Type:       exchanges.OrderTypeMarket,
			Quantity:   qty,
			Slippage:   slippage,
			ReduceOnly: true,
		})
		errCh <- closeLegResult{leg: "short", order: order, err: err}
	}()

	wg.Wait()
	close(errCh)

	var failures []string
	results := make(map[string]closeLegResult, 2)
	for res := range errCh {
		results[res.leg] = res
		if res.err == nil {
			continue
		}
		t.logger.Errorf("%s ❌ close %s leg: %v", t.roundTag(), res.leg, res.err)
		failures = append(failures, fmt.Sprintf("%s leg: %v", res.leg, res.err))
	}

	// Retry failed legs once before going to ManualIntervention.
	if len(failures) > 0 {
		failures = nil // reset for retry
		t.logger.Infof("%s 🔄 retrying failed close legs", t.roundTag())
		retryCtx, retryCancel := context.WithTimeout(closeCtx, 15*time.Second)
		defer retryCancel()

		for _, leg := range []string{"long", "short"} {
			res := results[leg]
			if res.err == nil {
				continue
			}
			var exchange exchanges.Exchange
			var side exchanges.OrderSide
			if leg == "long" {
				exchange, side = longExchange, exchanges.OrderSideSell
			} else {
				exchange, side = shortExchange, exchanges.OrderSideBuy
			}
			retryOrder, retryErr := exchange.PlaceOrder(retryCtx, &exchanges.OrderParams{
				Symbol:     t.config.Symbol,
				Side:       side,
				Type:       exchanges.OrderTypeMarket,
				Quantity:   qty,
				Slippage:   slippage,
				ReduceOnly: true,
			})
			if retryErr != nil {
				t.logger.Errorf("%s ❌ %s leg retry: %v", t.roundTag(), leg, retryErr)
				failures = append(failures, fmt.Sprintf("%s leg (retry): %v", leg, retryErr))
			} else {
				t.logger.Infof("%s ✅ %s leg retry ok", t.roundTag(), leg)
				results[leg] = closeLegResult{leg: leg, order: retryOrder}
				// Verify retry via WatchOrders for the leg
				if leg == "long" {
					if confirmed, _ := t.verifyCloseLeg(retryCtx, retryOrder, t.makerOrderCh, t.takerOrderCh, pos.Direction == LongMakerShortTaker); !confirmed {
						failures = append(failures, fmt.Sprintf("%s leg retry unconfirmed", leg))
					}
				} else {
					if confirmed, _ := t.verifyCloseLeg(retryCtx, retryOrder, t.makerOrderCh, t.takerOrderCh, pos.Direction == LongTakerShortMaker); !confirmed {
						failures = append(failures, fmt.Sprintf("%s leg retry unconfirmed", leg))
					}
				}
			}
		}
	}

	if len(failures) > 0 {
		var residualLeg string
		if results["long"].err == nil && results["short"].err != nil {
			residualLeg = "short"
		} else if results["short"].err == nil && results["long"].err != nil {
			residualLeg = "long"
		}

		t.mu.Lock()
		if t.position != nil && residualLeg != "" {
			switch residualLeg {
			case "long":
				t.position.ShortOrder = nil
			case "short":
				t.position.LongOrder = nil
			}
		}
		t.state = StateManualIntervention
		t.mu.Unlock()
		t.logger.Errorf("%s ❌ CLOSE FAILED residual=%s: %s", t.roundTag(), residualLeg, strings.Join(failures, "; "))
		go telegram.Notify(fmt.Sprintf(
			"🚨 %s CLOSE FAILED\nResidual: %s\nQty: %s\n%s",
			t.roundTag(), residualLeg, pos.OpenQuantity, strings.Join(failures, "; "),
		))
		return
	}

	longCloseOrder := t.refreshOrderForMetrics(ctx, longExchange, results["long"].order)
	shortCloseOrder := t.refreshOrderForMetrics(ctx, shortExchange, results["short"].order)
	t.logRealizedCloseMetrics(pos, longCloseOrder, shortCloseOrder)

	t.finishSuccessfulClose()

	t.logger.Infof("%s ✅ closed  %s  round=%d/%d (%dms)",
		t.roundTag(), reason, t.completedRounds, t.config.MaxRounds,
		time.Since(closeStart).Milliseconds())
	go telegram.Notify(fmt.Sprintf("🔴 %s Closed\n%s\nHeld: %s",
		t.roundTag(), reason, time.Since(pos.OpenTime).Round(time.Second)))
}

// verifyCloseLeg verifies a close leg order via WatchOrders channel.
func (t *Trader) verifyCloseLeg(ctx context.Context, order *exchanges.Order, makerCh, takerCh chan *exchanges.Order, useMaker bool) (bool, error) {
	if order == nil {
		return false, fmt.Errorf("nil order")
	}

	ch := takerCh
	exchange := t.taker
	if useMaker {
		ch = makerCh
		exchange = t.maker
	}

	_, err := t.confirmOrderFilled(ctx, exchange, order, ch, closeLegVerifyTimeout)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (t *Trader) confirmOrderFilled(ctx context.Context, exchange exchanges.Exchange, order *exchanges.Order, ch <-chan *exchanges.Order, timeout time.Duration) (*exchanges.Order, error) {
	if order == nil {
		return nil, fmt.Errorf("nil order")
	}
	if order.Status == exchanges.OrderStatusFilled {
		return order, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case update := <-ch:
			if update == nil {
				continue
			}
			if update.OrderID != order.OrderID && update.ClientOrderID != order.ClientOrderID {
				continue
			}
			mergeOrderDetails(order, update)
			if update.Status == exchanges.OrderStatusFilled {
				return order, nil
			}
			if update.Status == exchanges.OrderStatusCancelled || update.Status == exchanges.OrderStatusRejected {
				return nil, fmt.Errorf("status=%s", update.Status)
			}
		case <-timer.C:
			snapshot, err := t.fetchOrderSnapshot(ctx, exchange, order)
			if err != nil {
				return nil, err
			}
			if snapshot == nil {
				return nil, fmt.Errorf("order %s not found after timeout", order.OrderID)
			}
			switch snapshot.Status {
			case exchanges.OrderStatusFilled:
				return snapshot, nil
			case exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
				return nil, fmt.Errorf("status=%s", snapshot.Status)
			default:
				return nil, fmt.Errorf("status=%s filled=%s", snapshot.Status, snapshot.FilledQuantity)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (t *Trader) finishSuccessfulClose() {
	t.mu.Lock()
	t.position = nil
	t.state = StateCooldown
	t.lastTrade = time.Now()
	t.completedRounds++
	t.mu.Unlock()

	// Refresh PnL after a successful round.
	if t.pnl != nil {
		ctx := t.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		t.pnl.OnRoundComplete(ctx)
	}
}

func (t *Trader) fetchOrderSnapshot(ctx context.Context, exchange exchanges.Exchange, order *exchanges.Order) (*exchanges.Order, error) {
	if exchange == nil || order == nil || order.OrderID == "" {
		return nil, nil
	}
	fetchCtx := ctx
	if fetchCtx == nil || fetchCtx.Err() != nil {
		fetchCtx = context.Background()
	}
	snapshotCtx, cancel := context.WithTimeout(fetchCtx, 5*time.Second)
	defer cancel()
	snapshot, err := exchange.FetchOrderByID(snapshotCtx, order.OrderID, t.config.Symbol)
	if err != nil {
		return nil, fmt.Errorf("fetch order %s: %w", order.OrderID, err)
	}
	mergeOrderDetails(order, snapshot)
	return order, nil
}

func (t *Trader) refreshOrderForMetrics(ctx context.Context, exchange exchanges.Exchange, order *exchanges.Order) *exchanges.Order {
	if order == nil {
		return nil
	}
	if order.Price.GreaterThan(decimal.Zero) && order.Status == exchanges.OrderStatusFilled {
		return order
	}
	snapshot, err := t.fetchOrderSnapshot(ctx, exchange, order)
	if err != nil {
		t.logger.Warnf("%s close order refresh failed %s: %v", t.roundTag(), order.OrderID, err)
		return order
	}
	if snapshot != nil {
		return snapshot
	}
	return order
}

func (t *Trader) logRealizedCloseMetrics(pos *ArbPosition, longCloseOrder, shortCloseOrder *exchanges.Order) {
	if pos == nil {
		return
	}
	var makerFee, takerFee FeeInfo
	if t.engine != nil {
		makerFee = t.engine.makerFee
		takerFee = t.engine.takerFee
	}
	metrics, err := calculateRealizedProfitMetrics(pos, t.config, makerFee, takerFee, longCloseOrder, shortCloseOrder)
	if err != nil {
		t.logger.Warnf("%s ⚠️ realized bps unavailable: %v", t.roundTag(), err)
		return
	}

	t.logger.Infof("%s 💹 realized  signal=%.1fbps entry=%.1fbps exit=%.1fbps gross=%.1fbps fee=%.1fbps net=%.1fbps",
		t.roundTag(), pos.OpenExpectedProfit, metrics.EntryBps, metrics.ExitBps, metrics.GrossBps, metrics.FeeBps, metrics.NetBps)
}

func mergeOrderDetails(dst, src *exchanges.Order) {
	if dst == nil || src == nil {
		return
	}
	if dst.OrderID == "" {
		dst.OrderID = src.OrderID
	}
	if dst.ClientOrderID == "" {
		dst.ClientOrderID = src.ClientOrderID
	}
	if dst.Symbol == "" {
		dst.Symbol = src.Symbol
	}
	if dst.Side == "" {
		dst.Side = src.Side
	}
	if dst.Type == "" {
		dst.Type = src.Type
	}
	if dst.Quantity.IsZero() {
		dst.Quantity = src.Quantity
	}
	if src.Price.GreaterThan(decimal.Zero) {
		dst.Price = src.Price
	}
	if src.FilledQuantity.GreaterThan(decimal.Zero) {
		dst.FilledQuantity = src.FilledQuantity
	}
	if src.Fee.GreaterThan(decimal.Zero) {
		dst.Fee = src.Fee
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if dst.Timestamp == 0 {
		dst.Timestamp = src.Timestamp
	}
	if src.ReduceOnly {
		dst.ReduceOnly = true
	}
	if src.TimeInForce != "" {
		dst.TimeInForce = src.TimeInForce
	}
}

func (t *Trader) releaseCooldownIfExpiredLocked(now time.Time) {
	if t.state != StateCooldown {
		return
	}
	if t.config == nil || t.config.Cooldown <= 0 || now.Sub(t.lastTrade) >= t.config.Cooldown {
		t.state = StateIdle
	}
}

func (t *Trader) canStartNextRound() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.canStartNextRoundLocked(time.Now())
}

func (t *Trader) canStartNextRoundLocked(now time.Time) bool {
	t.releaseCooldownIfExpiredLocked(now)
	if t.state != StateIdle {
		return false
	}
	if t.config != nil && !t.config.DryRun && t.config.LiveValidate && t.completedRounds >= t.config.MaxRounds {
		return false
	}
	return true
}

// HasPosition returns whether the trader has an open position.
func (t *Trader) HasPosition() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.position != nil
}

// GracefulShutdown cancels pending maker orders and alerts about open positions.
// Called after the parent context is cancelled, uses a fresh context with deadline
// so exchange API calls are not immediately rejected.
func (t *Trader) GracefulShutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	t.mu.Lock()
	state := t.state
	flow := t.openFlow
	pos := t.position
	t.mu.Unlock()

	t.logger.Infof("🛑 graceful shutdown: state=%s hasPosition=%v hasOpenFlow=%v", state, pos != nil, flow != nil)

	// Step 1: Cancel any pending maker order
	if flow != nil && flow.makerOrder != nil {
		t.logger.Infof("🛑 cancelling pending maker order %s", flow.makerOrder.OrderID)
		if err := t.maker.CancelOrder(ctx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("🛑 maker cancel failed: %v", err)
		} else {
			t.logger.Infof("🛑 maker order cancelled")
		}
	}

	// Step 2: Belt-and-suspenders — cancel ALL open orders on both exchanges
	if !t.config.DryRun {
		t.cancelAllOpenOrders(ctx)
	}

	// Step 3: Close any open position using a fresh shutdown-safe context.
	if pos != nil {
		t.logger.Warnf("🛑 shutdown with open position, attempting forced close")
		t.closePosition("shutdown signal")
		t.mu.Lock()
		pos = t.position
		state = t.state
		t.mu.Unlock()
		if pos != nil {
			msg := fmt.Sprintf("🛑 SHUTDOWN with OPEN POSITION\n"+
				"Direction: %s\nQty: %s\nLong: %s / Short: %s\n"+
				"Opened: %s ago\nSpread: %.1f bps\nState: %s\n\n"+
				"⚠️ Auto-close failed — manual intervention required!",
				pos.Direction, pos.OpenQuantity,
				pos.LongExchange, pos.ShortExchange,
				time.Since(pos.OpenTime).Round(time.Second),
				pos.OpenSpread, state)
			t.logger.Errorf(msg)
			telegram.Notify(msg) // synchronous — we're shutting down
		} else {
			t.logger.Infof("🛑 shutdown position close succeeded")
		}
	}

	// Step 4: Report in-flight open flow
	if flow != nil && IsOpenFlowState(state) {
		// Check if maker order was partially filled
		filledQty := decimal.Zero
		if flow.makerOrder != nil {
			order, err := t.maker.FetchOrderByID(ctx, flow.makerOrder.OrderID, t.config.Symbol)
			if err == nil && order != nil {
				filledQty = order.FilledQuantity
			}
		}
		if filledQty.GreaterThan(decimal.Zero) {
			msg := fmt.Sprintf("🛑 SHUTDOWN with PARTIALLY FILLED maker order\n"+
				"OrderID: %s\nFilled: %s / %s\nHedged: %s\n\n"+
				"⚠️ Manual intervention required — possible unhedged exposure!",
				flow.makerOrder.OrderID, filledQty, flow.makerQty, flow.hedgedQty)
			t.logger.Errorf(msg)
			telegram.Notify(msg)
		}
	}

	t.logger.Infof("🛑 graceful shutdown complete")
}

// cancelAllOpenOrders cancels all open orders on both exchanges as a safety measure.
func (t *Trader) cancelAllOpenOrders(ctx context.Context) {
	for _, pair := range []struct {
		name string
		ex   exchanges.Exchange
	}{
		{t.config.MakerExchange, t.maker},
		{t.config.TakerExchange, t.taker},
	} {
		if err := pair.ex.CancelAllOrders(ctx, t.config.Symbol); err != nil {
			t.logger.Warnf("🛑 cancel all orders on %s failed: %v", pair.name, err)
		} else {
			t.logger.Infof("🛑 cancelled all orders on %s", pair.name)
		}
	}
}
