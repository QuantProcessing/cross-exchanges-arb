package trading

import (
	"context"
	"fmt"
	"sync"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/runlog"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/exchanges/account"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// ArbPosition represents an open arbitrage position.
type ArbPosition struct {
	Direction          spread.Direction
	OpenSpread         float64
	OpenZScore         float64
	OpenExpectedProfit float64
	OpenTime           time.Time
	OpenQuantity       decimal.Decimal
	LongOrder          *exchanges.Order
	ShortOrder         *exchanges.Order
	LongExchange       string
	ShortExchange      string
	OpenRecap          *openTradeRecap
}

// MarketDataEngine is the subset of spread-engine behavior the trader depends on.
type MarketDataEngine interface {
	SetMarketUpdateCallback(func())
	Snapshot() *spread.Snapshot
	RoundTripFeeBps() float64
	CurrentZ() (zAB, zBA float64)
	Fees() (maker spread.FeeInfo, taker spread.FeeInfo)
}

// Trader executes arbitrage trades based on signals from the spread engine.
type Trader struct {
	maker        exchanges.Exchange
	taker        exchanges.Exchange
	makerAccount *account.TradingAccount
	takerAccount *account.TradingAccount
	engine       MarketDataEngine
	config       *appconfig.Config
	logger       *zap.SugaredLogger

	mu              sync.Mutex
	position        *ArbPosition
	lastTrade       time.Time
	lastSignalTime  time.Time
	lastLogTime     time.Time
	state           State
	completedRounds int
	roundID         int
	openFlow        *openFlowState
	ctx             context.Context
	pnl             *PnLTracker
	events          *runlog.EventSink
	makerOrderCh    chan *exchanges.Order
	takerOrderCh    chan *exchanges.Order
	marketUpdateCh  chan struct{}
	orderTraces     map[string]*orderTrace
	makerOrdersSub  *account.Subscription[exchanges.Order]
	takerOrdersSub  *account.Subscription[exchanges.Order]
	makerFillCh     chan *exchanges.Fill
	takerFillCh     chan *exchanges.Fill
	makerFillsOnce  sync.Once
	takerFillsOnce  sync.Once
}

var closeLegVerifyTimeout = 5 * time.Second

func (t *Trader) roundTag() string {
	return fmt.Sprintf("R%03d", t.roundID)
}

func (t *Trader) evtInfof(format string, args ...any) {
	if t == nil || t.logger == nil {
		return
	}
	args = append([]any{t.roundTag()}, args...)
	t.logger.Infof("%s EVT "+format, args...)
}

func (t *Trader) evtErrorf(format string, args ...any) {
	if t == nil || t.logger == nil {
		return
	}
	args = append([]any{t.roundTag()}, args...)
	t.logger.Errorf("%s EVT "+format, args...)
}

func NewTrader(maker, taker exchanges.Exchange, makerAccount, takerAccount *account.TradingAccount, engine MarketDataEngine, cfg *appconfig.Config, logger *zap.SugaredLogger) *Trader {
	tr := &Trader{
		maker:          maker,
		taker:          taker,
		makerAccount:   makerAccount,
		takerAccount:   takerAccount,
		engine:         engine,
		config:         cfg,
		logger:         logger,
		makerOrderCh:   make(chan *exchanges.Order, 100),
		takerOrderCh:   make(chan *exchanges.Order, 100),
		marketUpdateCh: make(chan struct{}, 1),
		orderTraces:    make(map[string]*orderTrace),
		state:          StateIdle,
	}
	if engine != nil {
		engine.SetMarketUpdateCallback(tr.notifyMarketUpdate)
	}
	return tr
}

func (t *Trader) SetPnLTracker(p *PnLTracker) {
	t.pnl = p
}

func (t *Trader) SetEventSink(events *runlog.EventSink) {
	if t == nil {
		return
	}
	t.events = events
}

func (t *Trader) recordRoundEvent(event runlog.Event) {
	if t == nil || t.events == nil {
		return
	}
	event.Category = "round"
	if t.config != nil {
		if event.MakerExchange == "" {
			event.MakerExchange = t.config.MakerExchange
		}
		if event.TakerExchange == "" {
			event.TakerExchange = t.config.TakerExchange
		}
		if event.Symbol == "" {
			event.Symbol = t.config.Symbol
		}
	}
	if err := t.events.Record(event); err != nil && t.logger != nil {
		t.logger.Warnw("event sink write failed", "category", event.Category, "type", event.Type, "err", err)
	}
}

func (t *Trader) Start(ctx context.Context) error {
	t.ctx = ctx

	if t.makerAccount != nil {
		t.makerOrdersSub = t.makerAccount.SubscribeOrders()
		go t.forwardAccountOrders(ctx, t.config.MakerExchange, t.makerOrdersSub, t.makerOrderCh)
	}
	if t.takerAccount != nil {
		t.takerOrdersSub = t.takerAccount.SubscribeOrders()
		go t.forwardAccountOrders(ctx, t.config.TakerExchange, t.takerOrdersSub, t.takerOrderCh)
	}

	go t.monitorLoop(ctx)
	return nil
}

func (t *Trader) forwardAccountOrders(ctx context.Context, exchange string, sub *account.Subscription[exchanges.Order], out chan<- *exchanges.Order) {
	if sub == nil {
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case order, ok := <-sub.C:
			if !ok || order == nil {
				continue
			}
			t.observeOrderUpdate(exchange, order)
			select {
			case out <- order:
			case <-ctx.Done():
				return
			}
		}
	}
}
