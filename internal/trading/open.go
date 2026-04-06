package trading

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/runlog"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/exchanges/account"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
)

type openFlowState struct {
	signal         *spread.Signal
	makerOrder     *exchanges.Order
	makerFlow      *account.OrderFlow
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

	signalSnapshot      *tradeBBO
	makerPlacedSnapshot *tradeBBO
	fillSnapshot        *tradeBBO
	hedgeSnapshot       *tradeBBO
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
	roundID := t.roundID
	roundTag := fmt.Sprintf("R%03d", t.roundID)
	qty := t.config.Quantity
	if sig.Quantity.GreaterThan(decimal.Zero) && sig.Quantity.LessThan(qty) {
		qty = sig.Quantity
	}
	t.logger.Infof("%s EVT signal dir=%s edge=%+.1fbps z=%.2f profit=%+.1fbps qty=%s",
		roundTag, sig.Direction, sig.SpreadBps, sig.ZScore, sig.ExpectedProfit, qty)

	t.state = StatePlacingMaker
	sigCopy := *sig
	signalTime := now
	t.mu.Unlock()

	t.recordRoundEvent(runlog.Event{
		At:                signalTime,
		Type:              "signal",
		Round:             roundID,
		State:             string(StatePlacingMaker),
		Direction:         string(sig.Direction),
		Quantity:          qty.String(),
		SpreadBps:         sig.SpreadBps,
		ZScore:            sig.ZScore,
		ExpectedProfitBps: sig.ExpectedProfit,
	})

	go t.openMakerTaker(&sigCopy, signalTime, roundID)
}

func (t *Trader) openMakerTaker(sig *spread.Signal, signalTime time.Time, roundID int) {
	if sig == nil {
		return
	}

	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	qty := t.config.Quantity
	if sig.Quantity.GreaterThan(decimal.Zero) && sig.Quantity.LessThan(qty) {
		qty = sig.Quantity
	}
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
	if qty.LessThan(details.MinQuantity) {
		t.logger.Infow("signal quantity below minimum tradable size",
			"signal_qty", sig.Quantity,
			"floored_qty", qty,
			"min_qty", details.MinQuantity,
		)
		t.resetOpenFlow(StateIdle)
		return
	}

	cid := exchanges.GenerateID()
	makerSubmitAt := time.Now()
	if t.makerAccount == nil {
		t.evtErrorf("manual_intervention reason=%q", "maker trading account is not configured")
		t.recordRoundEvent(runlog.Event{
			At:       makerSubmitAt,
			Type:     "manual_intervention",
			Round:    roundID,
			State:    string(StateManualIntervention),
			Reason:   "maker trading account is not configured",
			Detail:   "open_maker",
			Quantity: qty.String(),
		})
		t.logger.Error("maker trading account is not configured")
		t.resetOpenFlow(StateManualIntervention)
		return
	}
	makerFlow, err := t.makerAccount.PlaceWS(ctx, &exchanges.OrderParams{
		Symbol:      t.config.Symbol,
		Side:        makerSide,
		Type:        exchanges.OrderTypePostOnly,
		Price:       makerPrice,
		Quantity:    qty,
		TimeInForce: exchanges.TimeInForcePO,
		ClientID:    cid,
	})
	if err != nil {
		t.logger.Errorf("%s maker placement failed: %v", t.roundTag(), err)
		go telegram.Notify(fmt.Sprintf("❌ Maker order failed: %v", err))
		t.resetOpenFlow(StateIdle)
		return
	}
	if orderFlowFills(makerFlow) == nil {
		t.ensureMakerFillNotifications()
	}
	makerOrder := makerFlow.Latest()
	if makerOrder == nil {
		makerOrder = &exchanges.Order{
			ClientOrderID: cid,
			Symbol:        t.config.Symbol,
			Side:          makerSide,
			Type:          exchanges.OrderTypePostOnly,
			Quantity:      qty,
			Price:         makerPrice,
			Status:        exchanges.OrderStatusPending,
		}
	}
	makerPlacedAt := time.Now()
	t.registerOrderTrace("open_maker", t.config.MakerExchange, makerOrder, makerSubmitAt, makerPlacedAt)

	t.mu.Lock()
	t.openFlow = &openFlowState{
		signal:              sig,
		makerOrder:          makerOrder,
		makerFlow:           makerFlow,
		makerPrice:          makerPrice,
		makerSide:           makerSide,
		takerSide:           takerSide,
		makerQty:            qty,
		signalTime:          signalTime,
		makerPlacedAt:       makerPlacedAt,
		signalSnapshot:      snapshotFromSignal(sig),
		makerPlacedSnapshot: t.captureCurrentTradeBBO(sig.Direction),
	}
	t.state = StateWaitingFill
	t.mu.Unlock()

	t.evtInfof("maker_placed side=%s ex=%s px=%s qty=%s wait=%dms",
		makerSide, t.config.MakerExchange, makerPrice, qty,
		makerPlacedAt.Sub(signalTime).Milliseconds())
	t.recordRoundEvent(runlog.Event{
		At:       makerPlacedAt,
		Type:     "maker_placed",
		Round:    roundID,
		State:    string(StateWaitingFill),
		Side:     string(makerSide),
		Exchange: t.config.MakerExchange,
		Price:    makerPrice.String(),
		Quantity: qty.String(),
		WaitMS:   makerPlacedAt.Sub(signalTime).Milliseconds(),
	})

	timeout := time.NewTimer(t.config.MakerTimeout)
	defer timeout.Stop()
	marketUpdateCh := t.ensureMarketUpdateNotifications()
	makerFlowOrders := orderFlowOrders(makerFlow)
	makerFlowFills := orderFlowFills(makerFlow)
	useFallbackMakerFills := makerFlowFills == nil
	if useFallbackMakerFills {
		makerFlowFills = t.ensureMakerFillNotifications()
	}

	if cancel, reason := t.shouldCancelPendingMaker(); cancel {
		if err := t.cancelPendingMaker(ctx, reason); err != nil {
			t.logger.Warnf("%s stale maker cancel failed: %v", t.roundTag(), err)
		}
		return
	}

	for {
		select {
		case update, ok := <-makerFlowOrders:
			if !ok {
				makerFlowOrders = nil
				continue
			}
			if done, err := t.handleMakerOrderUpdate(ctx, update, makerOrder, cid, qty); err != nil {
				t.logger.Errorf("%s maker update error: %v", t.roundTag(), err)
				return
			} else if done {
				return
			}
		case fill, ok := <-makerFlowFills:
			if !ok {
				makerFlowFills = nil
				continue
			}
			if !matchesOrderFill(makerOrder, fill) {
				continue
			}
			t.observeFill(t.config.MakerExchange, fill)
			if useFallbackMakerFills {
				synthetic := mergeFillDetails(makerOrder, fill)
				if done, err := t.handleMakerOrderUpdate(ctx, synthetic, makerOrder, cid, qty); err != nil {
					t.logger.Errorf("%s maker fill error: %v", t.roundTag(), err)
					return
				} else if done {
					return
				}
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

	resolvedOrder, err := t.resolveCancelableMakerOrder(ctx, flow)
	if err != nil {
		t.logger.Errorf("%s ❌ unable to resolve maker order for cancel: %v", t.roundTag(), err)
		go telegram.Notify(fmt.Sprintf("🚨 Maker order cancel unresolved — manual intervention required\nClientID: %s\nErr: %v",
			flow.makerOrder.ClientOrderID, err))
		t.mu.Lock()
		t.state = StateManualIntervention
		t.mu.Unlock()
		t.evtErrorf("manual_intervention reason=%q", "unable to resolve maker order for cancel")
		return err
	}
	flow.makerOrder = resolvedOrder

	t.logger.Infof("%s 📴 cancelling maker order %s: %s", t.roundTag(), flow.makerOrder.OrderID, reason)

	if flow.makerOrder != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cancelErr := error(nil)
		if t.makerAccount != nil {
			cancelErr = t.makerAccount.CancelWS(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol)
		} else {
			cancelErr = t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol)
		}
		if cancelErr != nil {
			t.logger.Warnf("%s cancel failed: %v", t.roundTag(), cancelErr)
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
			t.evtErrorf("manual_intervention reason=%q", "maker order status unknown after timeout")
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
		cancelErr := error(nil)
		if t.makerAccount != nil {
			cancelErr = t.makerAccount.CancelWS(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol)
		} else {
			cancelErr = t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol)
		}
		if cancelErr != nil {
			t.logger.Warnf("%s partial fill cancel failed: %v", t.roundTag(), cancelErr)
		}
		cancel()
	}

	if filledQty.GreaterThan(decimal.Zero) {
		t.recordFillSnapshot(flow)
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

func (t *Trader) resolveCancelableMakerOrder(ctx context.Context, flow *openFlowState) (*exchanges.Order, error) {
	if flow == nil || flow.makerOrder == nil {
		return nil, fmt.Errorf("maker order missing")
	}
	if flow.makerOrder.OrderID != "" {
		return flow.makerOrder, nil
	}
	if flow.makerFlow == nil {
		return nil, fmt.Errorf("maker order id unavailable for client %s", flow.makerOrder.ClientOrderID)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	order, err := flow.makerFlow.Wait(waitCtx, func(order *exchanges.Order) bool {
		return order != nil && order.OrderID != ""
	})
	if err == nil && order != nil {
		mergeOrderDetails(flow.makerOrder, order)
		return flow.makerOrder, nil
	}

	if latest := flow.makerFlow.Latest(); latest != nil {
		mergeOrderDetails(flow.makerOrder, latest)
		if flow.makerOrder.OrderID != "" {
			return flow.makerOrder, nil
		}
	}

	return nil, fmt.Errorf("maker order id unavailable for client %s", flow.makerOrder.ClientOrderID)
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
		stddevSpreadBps  float64
		valid            bool
	)
	switch makerSide {
	case exchanges.OrderSideBuy:
		valid = snapshot.ValidAB
		currentSpreadBps = snapshot.SpreadAB
		meanSpreadBps = snapshot.MeanAB
		stddevSpreadBps = snapshot.StdDevAB
	case exchanges.OrderSideSell:
		valid = snapshot.ValidBA
		currentSpreadBps = snapshot.SpreadBA
		meanSpreadBps = snapshot.MeanBA
		stddevSpreadBps = snapshot.StdDevBA
	default:
		return false, ""
	}
	if !valid {
		return true, "entry edge invalidated by non-executable quote"
	}

	requiredEdge := t.engine.RoundTripFeeBps() + t.config.MinProfitBps + t.config.ImpactBufferBps + t.config.LatencyBufferBps
	dynamicThreshold := meanSpreadBps + t.config.ZOpen*stddevSpreadBps
	if currentSpreadBps > requiredEdge && currentSpreadBps >= dynamicThreshold {
		return false, ""
	}

	return true, fmt.Sprintf(
		"entry edge invalidated current=%.2fbps dynamic>=%.2fbps required>%.2fbps",
		currentSpreadBps, dynamicThreshold, requiredEdge,
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
	var ordersCh <-chan *exchanges.Order = t.makerOrderCh
	var fillsCh <-chan *exchanges.Fill
	useFallbackFills := false
	if flow != nil && flow.makerFlow != nil {
		ordersCh = orderFlowOrders(flow.makerFlow)
		fillsCh = orderFlowFills(flow.makerFlow)
	}
	if fillsCh == nil {
		fillsCh = t.ensureMakerFillNotifications()
		useFallbackFills = true
	}
	for {
		select {
		case update, ok := <-ordersCh:
			if !ok {
				ordersCh = nil
				continue
			}
			done, err := t.handleMakerOrderUpdate(ctx, update, flow.makerOrder, "", flow.makerQty)
			if err != nil {
				return false, err
			}
			if done {
				return true, nil
			}
		case fill, ok := <-fillsCh:
			if !ok {
				fillsCh = nil
				continue
			}
			if flow == nil || !matchesOrderFill(flow.makerOrder, fill) {
				continue
			}
			t.observeFill(t.config.MakerExchange, fill)
			if useFallbackFills {
				done, err := t.handleMakerOrderUpdate(ctx, mergeFillDetails(flow.makerOrder, fill), flow.makerOrder, "", flow.makerQty)
				if err != nil {
					return false, err
				}
				if done {
					return true, nil
				}
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
	t.mu.Unlock()

	targetQty := update.FilledQuantity
	if targetQty.IsZero() && update.Status == exchanges.OrderStatusFilled {
		targetQty = flowQty
	}

	if targetQty.GreaterThan(decimal.Zero) {
		t.recordFillSnapshot(flow)
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
				t.openFlow.hedgeSnapshot = t.captureCurrentTradeBBO(t.openFlow.signal.Direction)
			}
			t.mu.Unlock()
			t.evtInfof("hedge_done filled=%s wait=%dms", targetQty, hedgeDone.Sub(hedgeStart).Milliseconds())
			t.recordRoundEvent(runlog.Event{
				At:       hedgeDone,
				Type:     "hedge_done",
				Round:    t.roundID,
				State:    string(StatePositionOpen),
				Exchange: t.config.TakerExchange,
				Side:     string(flow.takerSide),
				Quantity: targetQty.String(),
				WaitMS:   hedgeDone.Sub(hedgeStart).Milliseconds(),
			})
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
	hedgeSubmitAt := time.Now()
	if t.takerAccount == nil {
		return nil, fmt.Errorf("taker trading account is not configured")
	}
	cid := exchanges.GenerateID()
	hedgeFlow, err := t.takerAccount.PlaceWS(ctx, &exchanges.OrderParams{
		Symbol:   t.config.Symbol,
		Side:     flow.takerSide,
		Type:     exchanges.OrderTypeMarket,
		Quantity: delta,
		Slippage: slippage,
		ClientID: cid,
	})
	if err != nil {
		t.evtErrorf("hedge_failed qty=%s err=%v", delta, err)
		t.recordRoundEvent(runlog.Event{
			At:       hedgeSubmitAt,
			Type:     "hedge_failed",
			Round:    t.roundID,
			State:    string(StateHedging),
			Quantity: delta.String(),
			Detail:   err.Error(),
		})
		go telegram.Notify(fmt.Sprintf("🚨 HEDGE FAILED — UNHEDGED EXPOSURE!\nQty: %s\nErr: %v", delta, err))
		t.mu.Lock()
		t.state = StateManualIntervention
		t.mu.Unlock()
		t.evtErrorf("manual_intervention reason=%q", fmt.Sprintf("hedge failed for qty=%s", delta))
		t.recordRoundEvent(runlog.Event{
			At:       time.Now(),
			Type:     "manual_intervention",
			Round:    t.roundID,
			State:    string(StateManualIntervention),
			Reason:   fmt.Sprintf("hedge failed for qty=%s", delta),
			Quantity: delta.String(),
		})
		return nil, err
	}
	if orderFlowFills(hedgeFlow) == nil {
		t.ensureTakerFillNotifications()
	}
	hedgeOrder := hedgeFlow.Latest()
	if hedgeOrder == nil {
		hedgeOrder = &exchanges.Order{
			ClientOrderID: cid,
			Symbol:        t.config.Symbol,
			Side:          flow.takerSide,
			Type:          exchanges.OrderTypeMarket,
			Quantity:      delta,
			Status:        exchanges.OrderStatusPending,
		}
	}
	t.registerOrderTrace("open_hedge", t.config.TakerExchange, hedgeOrder, hedgeSubmitAt, time.Now())

	if confirmed, verifyErr := t.waitTakerFill(ctx, hedgeFlow, hedgeOrder, delta); !confirmed {
		errMsg := fmt.Sprintf("taker order rejected/unfilled: %v", verifyErr)
		t.evtErrorf("hedge_failed qty=%s err=%s", delta, errMsg)
		t.recordRoundEvent(runlog.Event{
			At:       time.Now(),
			Type:     "hedge_failed",
			Round:    t.roundID,
			State:    string(StateHedging),
			Quantity: delta.String(),
			Detail:   errMsg,
		})
		go telegram.Notify(fmt.Sprintf("🚨 HEDGE REJECTED — UNHEDGED!\nQty: %s\n%s", delta, errMsg))
		t.mu.Lock()
		t.state = StateManualIntervention
		t.mu.Unlock()
		t.evtErrorf("manual_intervention reason=%q", fmt.Sprintf("hedge rejected for qty=%s", delta))
		t.recordRoundEvent(runlog.Event{
			At:       time.Now(),
			Type:     "manual_intervention",
			Round:    t.roundID,
			State:    string(StateManualIntervention),
			Reason:   fmt.Sprintf("hedge rejected for qty=%s", delta),
			Quantity: delta.String(),
		})
		return nil, fmt.Errorf("%s", errMsg)
	}

	t.mu.Lock()
	if t.openFlow != nil && targetQty.GreaterThan(t.openFlow.hedgedQty) {
		t.openFlow.hedgedQty = targetQty
	}
	t.mu.Unlock()

	if targetQty.LessThan(flow.makerQty) && makerOrder != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cancelErr := error(nil)
		if t.makerAccount != nil {
			cancelErr = t.makerAccount.CancelWS(cancelCtx, makerOrder.OrderID, t.config.Symbol)
		} else {
			cancelErr = t.maker.CancelOrder(cancelCtx, makerOrder.OrderID, t.config.Symbol)
		}
		if cancelErr != nil {
			t.logger.Warnf("%s partial cancel failed: %v", t.roundTag(), cancelErr)
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

func (t *Trader) waitTakerFill(ctx context.Context, flow *account.OrderFlow, order *exchanges.Order, expectedDelta decimal.Decimal) (bool, error) {
	if order == nil {
		return false, fmt.Errorf("nil order")
	}

	timeout := time.After(10 * time.Second)
	var ordersCh <-chan *exchanges.Order = t.takerOrderCh
	if flow != nil {
		ordersCh = orderFlowOrders(flow)
	}
	fillsCh := orderFlowFills(flow)
	useFallbackFills := fillsCh == nil
	if useFallbackFills {
		fillsCh = t.ensureTakerFillNotifications()
	}
	for {
		select {
		case update, ok := <-ordersCh:
			if !ok {
				ordersCh = nil
				continue
			}
			if update == nil {
				continue
			}
			if update.OrderID != order.OrderID && update.ClientOrderID != order.ClientOrderID {
				continue
			}
			mergeOrderDetails(order, update)
			switch update.Status {
			case exchanges.OrderStatusFilled:
				return true, nil
			case exchanges.OrderStatusPartiallyFilled:
				continue
			case exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
				return false, fmt.Errorf("status=%s", update.Status)
			default:
				continue
			}
		case fill, ok := <-fillsCh:
			if !ok {
				fillsCh = nil
				continue
			}
			if !matchesOrderFill(order, fill) {
				continue
			}
			t.observeFill(t.config.TakerExchange, fill)
			if useFallbackFills {
				mergeFillDetails(order, fill)
				if order.Status == exchanges.OrderStatusFilled {
					return true, nil
				}
				continue
			}
			if latest := flow.Latest(); latest != nil {
				mergeOrderDetails(order, latest)
				switch latest.Status {
				case exchanges.OrderStatusFilled:
					return true, nil
				case exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
					return false, fmt.Errorf("status=%s", latest.Status)
				}
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
	t.mu.Lock()
	flow := t.openFlow
	totalExpected := expectedDelta
	if flow != nil {
		totalExpected = flow.hedgedQty.Add(expectedDelta)
	}
	t.mu.Unlock()

	if t.takerAccount != nil {
		for _, p := range t.takerAccount.Positions() {
			if strings.EqualFold(p.Symbol, t.config.Symbol) {
				absQty := p.Quantity.Abs()
				if absQty.GreaterThanOrEqual(totalExpected) {
					return true, nil
				}
				return false, fmt.Errorf("taker position %s < expected %s", absQty, totalExpected)
			}
		}
		return false, fmt.Errorf("no taker position found after hedge")
	}

	perp, ok := t.taker.(exchanges.PerpExchange)
	if !ok {
		return true, nil
	}

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
	if makerOrder != nil {
		mergeOrderDetails(makerOrder, flow.makerOrder)
		flow.makerOrder = makerOrder
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
		OpenRecap:          t.buildOpenTradeRecap(flow, filledQty),
	}
	t.lastTrade = time.Now()
	t.state = StatePositionOpen
	t.openFlow = nil
	t.mu.Unlock()

	if t.position != nil && t.position.OpenRecap != nil {
		t.logger.Infof("%s 📘 recap %s", t.roundTag(), t.position.OpenRecap.Summary())
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

func (t *Trader) recordFillSnapshot(flow *openFlowState) {
	if flow == nil || flow.signal == nil {
		return
	}

	now := time.Now()
	snapshot := t.captureCurrentTradeBBO(flow.signal.Direction)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.openFlow == nil || t.openFlow.signal == nil {
		return
	}
	if t.openFlow.fillDetectedAt.IsZero() {
		t.openFlow.fillDetectedAt = now
	}
	if t.openFlow.fillSnapshot == nil {
		t.openFlow.fillSnapshot = snapshot
	}
}
