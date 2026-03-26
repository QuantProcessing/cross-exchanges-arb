package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// ArbPosition represents an open arbitrage position.
type ArbPosition struct {
	Direction  SpreadDirection
	OpenSpread float64 // spread BPS at open
	OpenZScore float64 // Z-Score at open
	OpenTime   time.Time

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

	mu        sync.Mutex
	position  *ArbPosition // nil = no open position
	lastTrade time.Time    // for cooldown enforcement
	state     ExecutionState
	openFlow  *openFlowState

	// For maker-taker mode: order update channel
	makerOrderCh chan *exchanges.Order
	takerOrderCh chan *exchanges.Order
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

// Start begins the trader's monitoring loops.
func (t *Trader) Start(ctx context.Context) {
	// Subscribe to order updates for maker-taker fill tracking
	if !t.config.DryRun {
		go t.maker.WatchOrders(ctx, func(o *exchanges.Order) {
			select {
			case t.makerOrderCh <- o:
			default:
			}
		})
		go t.taker.WatchOrders(ctx, func(o *exchanges.Order) {
			select {
			case t.takerOrderCh <- o:
			default:
			}
		})
	}

	// Position monitor loop (check close conditions periodically)
	go t.monitorLoop(ctx)
}

type openFlowState struct {
	signal     *SpreadSignal
	makerOrder *exchanges.Order
	makerSide  exchanges.OrderSide
	takerSide  exchanges.OrderSide
	makerQty   decimal.Decimal
}

// HandleSignal processes a signal from the SpreadEngine.
func (t *Trader) HandleSignal(sig *SpreadSignal) {
	if sig == nil {
		return
	}

	t.mu.Lock()
	if !IsExecutableSignal(t.state, DefaultExecutionProfile(), sig) {
		t.mu.Unlock()
		return
	}

	// Already have open position? Check if close signal.
	if t.position != nil {
		t.mu.Unlock()
		return // close is handled by monitorLoop
	}

	// Cooldown check
	if time.Since(t.lastTrade) < t.config.Cooldown {
		t.mu.Unlock()
		return
	}

	t.logger.Infow("🟢 SIGNAL detected",
		"direction", sig.Direction,
		"spread", fmt.Sprintf("%.2f bps", sig.SpreadBps),
		"Z", fmt.Sprintf("%.2f", sig.ZScore),
		"expectedProfit", fmt.Sprintf("%.2f bps", sig.ExpectedProfit),
		"makerAsk", sig.MakerAsk,
		"takerBid", sig.TakerBid,
	)

	if t.config.DryRun {
		t.logger.Infow("🔸 [DRY RUN] Would open position",
			"direction", sig.Direction,
			"qty", t.config.Quantity,
		)
		t.position = &ArbPosition{
			Direction:  sig.Direction,
			OpenSpread: sig.SpreadBps,
			OpenZScore: sig.ZScore,
			OpenTime:   time.Now(),
		}
		t.lastTrade = time.Now()
		t.state = StatePositionOpen
		t.mu.Unlock()
		return
	}

	t.state = StatePlacingMaker
	sigCopy := *sig
	t.mu.Unlock()

	// Execute maker-taker asynchronously so HandleSignal only performs the idle -> placing_maker transition.
	go t.openMakerTaker(&sigCopy)
}

// openMakerTaker places a maker order on maker exchange, then hedges via taker on fill.
func (t *Trader) openMakerTaker(sig *SpreadSignal) {
	if sig == nil {
		return
	}

	ctx := context.Background()

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
		t.logger.Errorw("❌ maker order failed", "err", err)
		go telegram.Notify(fmt.Sprintf("❌ Maker order failed: %v", err))
		t.resetOpenFlow(StateIdle)
		return
	}

	t.mu.Lock()
	t.openFlow = &openFlowState{
		signal:     sig,
		makerOrder: makerOrder,
		makerSide:  makerSide,
		takerSide:  takerSide,
		makerQty:   qty,
	}
	t.state = StateWaitingFill
	t.mu.Unlock()

	t.logger.Infow("📌 Maker order placed, waiting for fill...",
		"exchange", t.config.MakerExchange,
		"side", makerSide,
		"price", makerPrice,
		"cid", cid,
	)

	// Wait for maker fill with timeout.
	timeout := time.NewTimer(t.config.MakerTimeout)
	defer timeout.Stop()

	for {
		select {
		case update := <-t.makerOrderCh:
			if !t.matchesOpenMakerOrder(update, makerOrder, cid) {
				continue
			}

			switch update.Status {
			case exchanges.OrderStatusFilled:
				filledQty := update.FilledQuantity
				if filledQty.IsZero() {
					filledQty = qty
				}
				if err := t.handleMakerFill(ctx, filledQty); err != nil {
					t.logger.Errorw("failed to hedge filled maker order", "err", err)
				}
				return
			case exchanges.OrderStatusCancelled, exchanges.OrderStatusRejected:
				filledQty := update.FilledQuantity
				if filledQty.GreaterThan(decimal.Zero) {
					if err := t.handleMakerFill(ctx, filledQty); err != nil {
						t.logger.Errorw("failed to hedge maker fill during cancel", "err", err)
					}
					return
				}
				t.resetOpenFlow(StateIdle)
				return
			case exchanges.OrderStatusPartiallyFilled:
				filledQty := update.FilledQuantity
				if filledQty.IsZero() {
					continue
				}
				if err := t.handleMakerFill(ctx, filledQty); err != nil {
					t.logger.Errorw("failed to hedge partial maker fill", "err", err)
				}
				return
			}
		case <-timeout.C:
			if err := t.handleMakerTimeout(ctx); err != nil {
				t.logger.Warnw("maker timeout handling failed", "err", err)
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

func (t *Trader) handleMakerFillForTest(filledQty decimal.Decimal) error {
	return t.handleMakerFill(context.Background(), filledQty)
}

func (t *Trader) handleMakerTimeoutForTest() {
	_ = t.handleMakerTimeout(context.Background())
}

func (t *Trader) handleMakerFill(ctx context.Context, filledQty decimal.Decimal) error {
	if filledQty.LessThanOrEqual(decimal.Zero) {
		return nil
	}

	t.mu.Lock()
	flow := t.openFlow
	if flow != nil {
		t.state = StateHedging
	}
	t.mu.Unlock()

	if flow == nil {
		t.mu.Lock()
		t.state = StatePositionOpen
		t.mu.Unlock()
		return nil
	}

	slippage := decimal.NewFromFloat(t.config.Slippage)
	hedgeOrder, err := exchanges.PlaceMarketOrderWithSlippage(
		ctx, t.taker, t.config.Symbol,
		flow.takerSide, filledQty, slippage,
	)
	if err != nil {
		t.logger.Errorw("❌ hedge order failed — UNHEDGED EXPOSURE!",
			"err", err, "qty", filledQty)
		go telegram.Notify(fmt.Sprintf("🚨 HEDGE FAILED — UNHEDGED EXPOSURE!\nQty: %s\nErr: %v", filledQty, err))
		t.mu.Lock()
		t.state = StateManualIntervention
		t.mu.Unlock()
		return err
	}

	if filledQty.LessThan(flow.makerQty) && flow.makerOrder != nil {
		go func(orderID string) {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.maker.CancelOrder(cancelCtx, orderID, t.config.Symbol)
		}(flow.makerOrder.OrderID)
	}

	longName, shortName := t.config.MakerExchange, t.config.TakerExchange
	if flow.signal.Direction == LongTakerShortMaker {
		longName, shortName = shortName, longName
	}

	t.mu.Lock()
	t.position = &ArbPosition{
		Direction:     flow.signal.Direction,
		OpenSpread:    flow.signal.SpreadBps,
		OpenZScore:    flow.signal.ZScore,
		OpenTime:      time.Now(),
		LongOrder:     flow.makerOrder,
		ShortOrder:    hedgeOrder,
		LongExchange:  longName,
		ShortExchange: shortName,
	}
	t.lastTrade = time.Now()
	t.state = StatePositionOpen
	t.openFlow = nil
	t.mu.Unlock()

	t.logger.Infow("✅ Position opened (maker-taker)",
		"long", longName, "short", shortName,
		"spread", fmt.Sprintf("%.2f bps", flow.signal.SpreadBps),
		"filled", filledQty,
	)
	go telegram.Notify(fmt.Sprintf("✅ Opened (maker-taker)\nLong %s / Short %s\nSpread: %.2f bps | Z: %.2f | Qty: %s",
		longName, shortName, flow.signal.SpreadBps, flow.signal.ZScore, filledQty))
	return nil
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

	if flow.makerOrder != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := t.maker.CancelOrder(cancelCtx, flow.makerOrder.OrderID, t.config.Symbol); err != nil {
			t.logger.Warnw("maker cancel failed after timeout", "err", err, "orderID", flow.makerOrder.OrderID)
		}
		cancel()
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filledQty, err := t.waitForMakerTerminalStatus(waitCtx, flow)
	if filledQty.GreaterThan(decimal.Zero) {
		return t.handleMakerFill(ctx, filledQty)
	}
	if err != nil {
		t.logger.Warnw("waiting for maker terminal status failed", "err", err)
		t.resetOpenFlow(StateIdle)
		return err
	}

	t.resetOpenFlow(StateIdle)
	return nil
}

func (t *Trader) waitForMakerTerminalStatus(ctx context.Context, flow *openFlowState) (decimal.Decimal, error) {
	filledQty := decimal.Zero

	for {
		select {
		case update := <-t.makerOrderCh:
			if !t.matchesOpenMakerOrder(update, flow.makerOrder, "") {
				continue
			}

			if update.FilledQuantity.GreaterThan(filledQty) {
				filledQty = update.FilledQuantity
			}
			if update.Status == exchanges.OrderStatusFilled {
				if filledQty.IsZero() {
					filledQty = flow.makerQty
				}
				return filledQty, nil
			}
			if update.Status == exchanges.OrderStatusCancelled || update.Status == exchanges.OrderStatusRejected {
				return filledQty, nil
			}
		case <-ctx.Done():
			return filledQty, ctx.Err()
		}
	}
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
		}
	}
}

// checkCloseConditions evaluates whether to close the current position.
func (t *Trader) checkCloseConditions() {
	t.mu.Lock()
	pos := t.position
	if pos == nil {
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
		// Log status periodically
		if holdTime > 0 && int(holdTime.Seconds())%10 == 0 {
			t.logger.Infow("📊 holding position",
				"direction", pos.Direction,
				"openSpread", fmt.Sprintf("%.2f bps", pos.OpenSpread),
				"currentZ", fmt.Sprintf("%.2f", currentZ),
				"holdTime", holdTime.Round(time.Second),
			)
		}
		return
	}

	t.closePosition(reason)
}

// closePosition closes the current position.
func (t *Trader) closePosition(reason string) {
	t.mu.Lock()
	pos := t.position
	if pos == nil {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	t.logger.Infow("🔴 Closing position",
		"reason", reason,
		"direction", pos.Direction,
		"holdTime", time.Since(pos.OpenTime).Round(time.Second),
	)

	if t.config.DryRun {
		t.logger.Infow("🔸 [DRY RUN] Would close position",
			"direction", pos.Direction,
			"openSpread", fmt.Sprintf("%.2f bps", pos.OpenSpread),
		)
		t.mu.Lock()
		t.position = nil
		t.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	qty := t.config.Quantity
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

	go func() {
		defer wg.Done()
		_, err := exchanges.PlaceMarketOrderWithSlippage(
			ctx, longExchange, t.config.Symbol,
			exchanges.OrderSideSell, qty, slippage,
		)
		if err != nil {
			t.logger.Errorw("❌ close long failed", "err", err)
		}
	}()

	go func() {
		defer wg.Done()
		_, err := exchanges.PlaceMarketOrderWithSlippage(
			ctx, shortExchange, t.config.Symbol,
			exchanges.OrderSideBuy, qty, slippage,
		)
		if err != nil {
			t.logger.Errorw("❌ close short failed", "err", err)
		}
	}()

	wg.Wait()

	t.mu.Lock()
	t.position = nil
	t.mu.Unlock()

	t.logger.Infow("✅ Position closed", "reason", reason)
	go telegram.Notify(fmt.Sprintf("🔴 Position closed\nReason: %s\nHeld: %s",
		reason, time.Since(pos.OpenTime).Round(time.Second)))
}

// HasPosition returns whether the trader has an open position.
func (t *Trader) HasPosition() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.position != nil
}
