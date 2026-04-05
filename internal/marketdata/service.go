package marketdata

import (
	"context"
	"fmt"
	"sync"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
)

type Service struct {
	maker       exchanges.Exchange
	taker       exchanges.Exchange
	symbol      string
	makerName   string
	takerName   string
	mu          sync.Mutex
	subscribers []func(MarketFrame)
}

func NewService(maker, taker exchanges.Exchange, symbol, makerName, takerName string) *Service {
	return &Service{
		maker:     maker,
		taker:     taker,
		symbol:    symbol,
		makerName: makerName,
		takerName: takerName,
	}
}

func (s *Service) Subscribe(cb func(MarketFrame)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, cb)
}

func (s *Service) Start(ctx context.Context) error {
	errCh := make(chan error, 2)

	start := func(side Side, ex exchanges.Exchange) {
		go func() {
			err := ex.WatchOrderBook(ctx, s.symbol, 2, func(_ *exchanges.OrderBook) {
				if frame, ok := s.snapshotFrame(side); ok {
					s.dispatch(frame)
				}
			})
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				errCh <- fmt.Errorf("%s WatchOrderBook: %w", side, err)
			}
		}()
	}

	start(SideMaker, s.maker)
	start(SideTaker, s.taker)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) snapshotFrame(updated Side) (MarketFrame, bool) {
	makerBook := s.maker.GetLocalOrderBook(s.symbol, 2)
	takerBook := s.taker.GetLocalOrderBook(s.symbol, 2)
	if !bookReady(makerBook) || !bookReady(takerBook) {
		return MarketFrame{}, false
	}

	return MarketFrame{
		SchemaVersion:   1,
		LocalTime:       time.Now().UTC(),
		UpdatedSide:     updated,
		Symbol:          s.symbol,
		MakerExchange:   s.makerName,
		TakerExchange:   s.takerName,
		MakerExchangeTS: orderBookTimestamp(makerBook.Timestamp),
		TakerExchangeTS: orderBookTimestamp(takerBook.Timestamp),
		MakerBook:       convertBook(makerBook),
		TakerBook:       convertBook(takerBook),
	}, true
}

func (s *Service) dispatch(frame MarketFrame) {
	s.mu.Lock()
	subs := append([]func(MarketFrame){}, s.subscribers...)
	s.mu.Unlock()
	for _, cb := range subs {
		cb(frame)
	}
}

func convertBook(ob *exchanges.OrderBook) MarketBook {
	return MarketBook{
		Bids: convertLevels(ob.Bids),
		Asks: convertLevels(ob.Asks),
	}
}

func convertLevels(levels []exchanges.Level) []MarketLevel {
	out := make([]MarketLevel, 0, len(levels))
	for _, level := range levels {
		out = append(out, MarketLevel{
			Price:    level.Price,
			Quantity: level.Quantity,
		})
	}
	return out
}

func bookReady(ob *exchanges.OrderBook) bool {
	return ob != nil && len(ob.Bids) >= 2 && len(ob.Asks) >= 2
}

func orderBookTimestamp(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	if ts > 1_000_000_000_000 {
		return time.UnixMilli(ts)
	}
	return time.Unix(ts, 0).UTC()
}
