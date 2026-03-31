package trading

import (
	"context"
	"fmt"
	"sync"
	"time"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	"github.com/QuantProcessing/cross-exchanges-arb/internal/spread"
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/notify/telegram"
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
	maker  exchanges.Exchange
	taker  exchanges.Exchange
	engine MarketDataEngine
	config *appconfig.Config
	logger *zap.SugaredLogger

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
	makerOrderCh    chan *exchanges.Order
	takerOrderCh    chan *exchanges.Order
	marketUpdateCh  chan struct{}
}

var closeLegVerifyTimeout = 5 * time.Second

func (t *Trader) roundTag() string {
	return fmt.Sprintf("R%03d", t.roundID)
}

func NewTrader(maker, taker exchanges.Exchange, engine MarketDataEngine, cfg *appconfig.Config, logger *zap.SugaredLogger) *Trader {
	tr := &Trader{
		maker:          maker,
		taker:          taker,
		engine:         engine,
		config:         cfg,
		logger:         logger,
		makerOrderCh:   make(chan *exchanges.Order, 100),
		takerOrderCh:   make(chan *exchanges.Order, 100),
		marketUpdateCh: make(chan struct{}, 1),
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

func (t *Trader) Start(ctx context.Context) error {
	t.ctx = ctx

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
