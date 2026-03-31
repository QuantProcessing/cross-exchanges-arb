package trading

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
)

type openFlowState struct {
	signal         *spread.Signal
	makerOrder     *exchanges.Order
	makerPrice     decimal.Decimal
	makerSide      exchanges.OrderSide
	takerSide      exchanges.OrderSide
	makerQty       decimal.Decimal
	hedgedQty      decimal.Decimal
	lastHedgeOrder *exchanges.Order

	signalTime     time.Time
	makerPlacedAt  time.Time
	fillDetectedAt time.Time
	hedgeDoneAt    time.Time
}

// HandleSignal processes a signal from the spread engine.
func (t *Trader) HandleSignal(sig *spread.Signal) {
	if sig == nil {
		return
	}
	t.ensureMarketUpdateNotifications()

	t.mu.Lock()
	now := time.Now()
	if !t.canStartNextRoundLocked(now) {
		t.mu.Unlock()
		return
	}

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

	go t.openMakerTaker(&sigCopy, signalTime)
}

func (t *Trader) openMakerTaker(sig *spread.Signal, signalTime time.Time) {
	if sig == nil {
		return
	}

	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	qty := t.config.Quantity
	var makerSide, takerSide exchanges.OrderSide
	var makerPrice decimal.Decimal

	if sig.Direction == spread.LongMakerShortTaker {
		makerSide = exchanges.OrderSideBuy
		takerSide = exchanges.OrderSideSell
		makerPrice = sig.MakerAsk
	} else {
		makerSide = exchanges.OrderSideSell
		takerSide = exchanges.OrderSideBuy
		makerPrice = sig.MakerBid
	}

	offset := makerPrice.Mul(decimal.NewFromFloat(0.0001))
	if makerSide == exchanges.OrderSideBuy {
		makerPrice = makerPrice.Sub(offset)
	} else {
		makerPrice = makerPrice.Add(offset)
	}

	details, err := t.maker.FetchSymbolDetails(ctx, t.config.Symbol)
	if err != nil {
		t.logger.Errorw("failed to fetch symbol details", "err", err)
		t.resetOpenFlow(StateIdle)
		return
	}
	makerPrice = exchanges.RoundToPrecision(makerPrice, details.PricePrecision)
	qty = exchanges.FloorToPrecision(qty, details.QuantityPrecision)

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
		makerPrice:    makerPrice,
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

	timeout := time.NewTimer(t.config.MakerTimeout)
	defer timeout.Stop()
	marketUpdateCh := t.ensureMarketUpdateNotifications()

	if cancel, reason := t.shouldCancelPendingMaker(); cancel {
		if err := t.cancelPendingMaker(ctx, reason); err != nil {
			t.logger.Warnf("%s stale maker cancel failed: %v", t.roundTag(), err)
		}
		return
	}

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
		case <-marketUpdateCh:
			if cancel, reason := t.shouldCancelPendingMaker(); cancel {
				if err := t.cancelPendingMaker(ctx, reason); err != nil {
					t.logger.Warnf("%s stale maker cancel failed: %v", t.roundTag(), err)
				}
				return
			}
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
	return t.cancelPendingMaker(ctx, "maker-timeout reached")
}

func (t *Trader) cancelPendingMaker(ctx context.Context, reason string) error {
	t.mu.Lock()
	flow := t.openFlow
	if flow == nil {
		t.state = StateIdle
		t.mu.Unlock()
		return nil
	}
	t.state = StateClosing
	t.mu.Unlock()

	t.logger.Infof("%s 📴 cancelling maker order %s: %s", t.roundTag(), flow.makerOrder.OrderID, reason)

	if flow.makerOrder != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("%s cancel failed: %v", t.roundTag(), err)
		}
		cancel()
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	settled, _ := t.settleMakerFlow(waitCtx, flow)
	cancel()
	if settled {
		return nil
	}

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

	if filledQty.GreaterThan(decimal.Zero) && filledQty.LessThan(flow.makerQty) &&
		orderStatus == exchanges.OrderStatusPartiallyFilled {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("%s partial fill cancel failed: %v", t.roundTag(), err)
		}
		cancel()
	}

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

func (t *Trader) ensureMarketUpdateNotifications() chan struct{} {
	t.mu.Lock()
	if t.marketUpdateCh == nil {
		t.marketUpdateCh = make(chan struct{}, 1)
	}
	ch := t.marketUpdateCh
	t.mu.Unlock()

	if t.engine != nil {
		t.engine.SetMarketUpdateCallback(t.notifyMarketUpdate)
	}
	return ch
}

func (t *Trader) notifyMarketUpdate() {
	ch := t.ensureMarketUpdateNotifications()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (t *Trader) shouldCancelPendingMaker() (bool, string) {
	t.mu.Lock()
	flow := t.openFlow
	if flow == nil || t.state != StateWaitingFill {
		t.mu.Unlock()
		return false, ""
	}
	makerPrice := flow.makerPrice
	makerSide := flow.makerSide
	t.mu.Unlock()

	if t.engine == nil {
		return false, ""
	}
	snapshot := t.engine.Snapshot()
	if snapshot == nil || makerPrice.IsZero() {
		return false, ""
	}

	var (
		currentSpreadBps float64
		meanSpreadBps    float64
	)
	switch makerSide {
	case exchanges.OrderSideBuy:
		if snapshot.TakerBid.IsZero() {
			return false, ""
		}
		midPrice := makerPrice.Add(snapshot.TakerBid).Div(decimal.NewFromInt(2))
		if midPrice.IsZero() {
			return false, ""
		}
		currentSpreadBps, _ = snapshot.TakerBid.Sub(makerPrice).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()
		meanSpreadBps = snapshot.MeanAB
	case exchanges.OrderSideSell:
		if snapshot.TakerAsk.IsZero() {
			return false, ""
		}
		midPrice := makerPrice.Add(snapshot.TakerAsk).Div(decimal.NewFromInt(2))
		if midPrice.IsZero() {
			return false, ""
		}
		currentSpreadBps, _ = makerPrice.Sub(snapshot.TakerAsk).Div(midPrice).Mul(decimal.NewFromInt(10000)).Float64()
		meanSpreadBps = snapshot.MeanBA
	default:
		return false, ""
	}

	requiredEdge := t.engine.RoundTripFeeBps() + t.config.MinProfitBps
	expectedGrossProfit := currentSpreadBps - meanSpreadBps
	if expectedGrossProfit > requiredEdge {
		return false, ""
	}

	return true, fmt.Sprintf(
		"entry edge invalidated current=%.2fbps mean=%.2fbps required>%.2fbps",
		currentSpreadBps, meanSpreadBps, requiredEdge,
	)
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
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("%s partial cancel failed: %v", t.roundTag(), err)
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
			if update.OrderID != order.OrderID && update.ClientOrderID != order.ClientOrderID {
				continue
			}
			mergeOrderDetails(order, update)
			switch update.Status {
			case exchanges.OrderStatusFilled:
				t.logger.Infof("%s ✅ taker confirmed FILLED", t.roundTag())
				return true, nil
			case exchanges.OrderStatusPartiallyFilled:
				continue
			case exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
				return false, fmt.Errorf("status=%s", update.Status)
			default:
				continue
			}
		case <-timeout:
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

func (t *Trader) verifyTakerPosition(ctx context.Context, expectedDelta decimal.Decimal) (bool, error) {
	perp, ok := t.taker.(exchanges.PerpExchange)
	if !ok {
		return true, nil
	}

	t.mu.Lock()
	flow := t.openFlow
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
	if flow.signal.Direction == spread.LongTakerShortMaker {
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

func (t *Trader) resetOpenFlow(state State) {
	t.mu.Lock()
	t.openFlow = nil
	t.state = state
	t.mu.Unlock()
}
