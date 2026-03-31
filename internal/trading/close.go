package trading

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
)

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

func (t *Trader) checkCloseConditions() {
	t.mu.Lock()
	pos := t.position
	if pos == nil || t.state != StatePositionOpen {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	zAB, zBA := t.engine.CurrentZ()

	var currentZ float64
	if pos.Direction == spread.LongMakerShortTaker {
		currentZ = zAB
	} else {
		currentZ = zBA
	}

	holdTime := time.Since(pos.OpenTime)
	reason := ""

	if currentZ < t.config.ZClose {
		reason = fmt.Sprintf("spread reverted (Z=%.2f < %.2f)", currentZ, t.config.ZClose)
	}
	if currentZ < t.config.ZStop {
		reason = fmt.Sprintf("stop loss (Z=%.2f < %.2f)", currentZ, t.config.ZStop)
	}
	if holdTime > t.config.MaxHoldTime {
		reason = fmt.Sprintf("max hold time exceeded (%s > %s)", holdTime.Round(time.Second), t.config.MaxHoldTime)
	}

	if reason == "" {
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
	if pos.Direction == spread.LongMakerShortTaker {
		longExchange, shortExchange = t.maker, t.taker
	} else {
		longExchange, shortExchange = t.taker, t.maker
	}

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

	if len(failures) > 0 {
		failures = nil
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
				if leg == "long" {
					if confirmed, _ := t.verifyCloseLeg(retryCtx, retryOrder, t.makerOrderCh, t.takerOrderCh, pos.Direction == spread.LongMakerShortTaker); !confirmed {
						failures = append(failures, fmt.Sprintf("%s leg retry unconfirmed", leg))
					}
				} else {
					if confirmed, _ := t.verifyCloseLeg(retryCtx, retryOrder, t.makerOrderCh, t.takerOrderCh, pos.Direction == spread.LongTakerShortMaker); !confirmed {
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
	var makerFee, takerFee spread.FeeInfo
	if t.engine != nil {
		makerFee, takerFee = t.engine.Fees()
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

func (t *Trader) HasPosition() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.position != nil
}

func (t *Trader) GracefulShutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	t.mu.Lock()
	state := t.state
	flow := t.openFlow
	pos := t.position
	t.mu.Unlock()

	t.logger.Infof("🛑 graceful shutdown: state=%s hasPosition=%v hasOpenFlow=%v", state, pos != nil, flow != nil)

	if flow != nil && flow.makerOrder != nil {
		t.logger.Infof("🛑 cancelling pending maker order %s", flow.makerOrder.OrderID)
		if err := t.maker.CancelOrder(ctx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnf("🛑 maker cancel failed: %v", err)
		} else {
			t.logger.Infof("🛑 maker order cancelled")
		}
	}

	if !t.config.DryRun {
		t.cancelAllOpenOrders(ctx)
	}

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
			telegram.Notify(msg)
		} else {
			t.logger.Infof("🛑 shutdown position close succeeded")
		}
	}

	if flow != nil && IsOpenFlowState(state) {
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
