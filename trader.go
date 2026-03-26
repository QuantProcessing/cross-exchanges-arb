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
	Direction    SpreadDirection
	OpenSpread   float64 // spread BPS at open
	OpenZScore   float64 // Z-Score at open
	OpenTime     time.Time
	OpenQuantity decimal.Decimal

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
func (t *Trader) Start(ctx context.Context) {
	t.ctx = ctx

	// Subscribe to order updates for maker-taker fill tracking
	if !t.config.DryRun {
		go t.maker.WatchOrders(ctx, func(o *exchanges.Order) {
			t.makerOrderCh <- o
		})
		go t.taker.WatchOrders(ctx, func(o *exchanges.Order) {
			t.takerOrderCh <- o
		})
	}

	// Position monitor loop (check close conditions periodically)
	go t.monitorLoop(ctx)
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

	now := time.Now()
	t.mu.Lock()
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
			Direction:    sig.Direction,
			OpenSpread:   sig.SpreadBps,
			OpenZScore:   sig.ZScore,
			OpenTime:     now,
			OpenQuantity: t.config.Quantity,
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
		makerPrice = sig.MakerAsk // buy at ask (or slightly below for maker)
	} else {
		makerSide = exchanges.OrderSideSell
		takerSide = exchanges.OrderSideBuy
		makerPrice = sig.MakerBid // sell at bid (or slightly above for maker)
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
	if flow.makerOrder != nil {
		restCtx, restCancel := context.WithTimeout(context.Background(), 5*time.Second)
		order, err := t.maker.FetchOrderByID(restCtx, flow.makerOrder.OrderID, t.config.Symbol)
		restCancel()
		if err != nil {
			t.logger.Errorf("%s REST fallback failed: %v", t.roundTag(), err)
			go telegram.Notify(fmt.Sprintf("⚠️ Maker order status unknown after timeout\nOrderID: %s\nErr: %v",
				flow.makerOrder.OrderID, err))
			t.resetOpenFlow(StateIdle)
			return err
		}
		if order != nil {
			filledQty = order.FilledQuantity
			t.logger.Infof("%s REST status=%s filled=%s", t.roundTag(), order.Status, filledQty)
		}
	}

	// Step 4: If any quantity was filled, hedge it; otherwise go idle.
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

	t.mu.Lock()
	if t.openFlow != nil && targetQty.GreaterThan(t.openFlow.hedgedQty) {
		t.openFlow.hedgedQty = targetQty
	}
	t.mu.Unlock()

	if targetQty.LessThan(flow.makerQty) && makerOrder != nil {
		go func(orderID string) {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := t.maker.CancelOrder(cancelCtx, orderID, t.config.Symbol); err != nil {
				t.logger.Warnf("%s partial cancel failed: %v", t.roundTag(), err)
			}
		}(makerOrder.OrderID)
	}

	t.mu.Lock()
	if t.openFlow != nil && targetQty.GreaterThanOrEqual(t.openFlow.hedgedQty) && targetQty.LessThan(t.openFlow.makerQty) {
		t.state = StateWaitingFill
	}
	t.mu.Unlock()

	return hedgeOrder, nil
}

func (t *Trader) finalizeOpenFlow(makerOrder *exchanges.Order, filledQty decimal.Decimal) {
	t.mu.Lock()
	flow := t.openFlow
	if flow == nil {
		t.mu.Unlock()
		return
	}

	longName, shortName := t.config.MakerExchange, t.config.TakerExchange
	if flow.signal.Direction == LongTakerShortMaker {
		longName, shortName = shortName, longName
	}

	t.position = &ArbPosition{
		Direction:     flow.signal.Direction,
		OpenSpread:    flow.signal.SpreadBps,
		OpenZScore:    flow.signal.ZScore,
		OpenTime:      time.Now(),
		OpenQuantity:  filledQty,
		LongOrder:     makerOrder,
		ShortOrder:    flow.lastHedgeOrder,
		LongExchange:  longName,
		ShortExchange: shortName,
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
	if closeCtx == nil {
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

	// Close both sides concurrently (ReduceOnly)
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
		order, err := exchanges.PlaceMarketOrderWithSlippage(
			ctx, longExchange, t.config.Symbol,
			exchanges.OrderSideSell, qty, slippage,
		)
		errCh <- closeLegResult{leg: "long", order: order, err: err}
	}()

	go func() {
		defer wg.Done()
		order, err := exchanges.PlaceMarketOrderWithSlippage(
			ctx, shortExchange, t.config.Symbol,
			exchanges.OrderSideBuy, qty, slippage,
		)
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
			retryOrder, retryErr := exchanges.PlaceMarketOrderWithSlippage(
				retryCtx, exchange, t.config.Symbol, side, qty, slippage,
			)
			if retryErr != nil {
				t.logger.Errorf("%s ❌ %s leg retry: %v", t.roundTag(), leg, retryErr)
				failures = append(failures, fmt.Sprintf("%s leg (retry): %v", leg, retryErr))
			} else {
				t.logger.Infof("%s ✅ %s leg retry ok", t.roundTag(), leg)
				results[leg] = closeLegResult{leg: leg, order: retryOrder}
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

	t.finishSuccessfulClose()

	t.logger.Infof("%s ✅ closed  %s  round=%d/%d (%dms)",
		t.roundTag(), reason, t.completedRounds, t.config.MaxRounds,
		time.Since(closeStart).Milliseconds())
	go telegram.Notify(fmt.Sprintf("🔴 %s Closed\n%s\nHeld: %s",
		t.roundTag(), reason, time.Since(pos.OpenTime).Round(time.Second)))
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
