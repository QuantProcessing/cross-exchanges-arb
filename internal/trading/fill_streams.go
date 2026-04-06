package trading

import (
	"context"
	"errors"

	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/shopspring/decimal"
)

func (t *Trader) ensureMakerFillNotifications() <-chan *exchanges.Fill {
	t.mu.Lock()
	if t.makerFillCh == nil {
		t.makerFillCh = make(chan *exchanges.Fill, 128)
	}
	ch := t.makerFillCh
	t.mu.Unlock()

	t.makerFillsOnce.Do(func() {
		t.startExchangeFillWatch(t.ctx, t.maker, t.config.MakerExchange, ch)
	})
	return ch
}

func (t *Trader) ensureTakerFillNotifications() <-chan *exchanges.Fill {
	t.mu.Lock()
	if t.takerFillCh == nil {
		t.takerFillCh = make(chan *exchanges.Fill, 128)
	}
	ch := t.takerFillCh
	t.mu.Unlock()

	t.takerFillsOnce.Do(func() {
		t.startExchangeFillWatch(t.ctx, t.taker, t.config.TakerExchange, ch)
	})
	return ch
}

func (t *Trader) startExchangeFillWatch(ctx context.Context, exchange exchanges.Exchange, exchangeName string, out chan<- *exchanges.Fill) {
	if exchange == nil || out == nil {
		return
	}

	streamable, ok := exchange.(exchanges.Streamable)
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	go func() {
		if err := streamable.WatchFills(ctx, func(fill *exchanges.Fill) {
			if fill == nil {
				return
			}
			select {
			case out <- fill:
			default:
			}
		}); err != nil && !errors.Is(err, exchanges.ErrNotSupported) {
			t.logger.Warnw("fallback fill watch failed", "exchange", exchangeName, "error", err)
		}
	}()
}

func matchesOrderFill(order *exchanges.Order, fill *exchanges.Fill) bool {
	if order == nil || fill == nil {
		return false
	}
	if order.OrderID != "" && fill.OrderID == order.OrderID {
		return true
	}
	if order.ClientOrderID != "" && fill.ClientOrderID == order.ClientOrderID {
		return true
	}
	return false
}

func mergeFillDetails(order *exchanges.Order, fill *exchanges.Fill) *exchanges.Order {
	if order == nil || fill == nil {
		return order
	}

	if order.OrderID == "" {
		order.OrderID = fill.OrderID
	}
	if order.ClientOrderID == "" {
		order.ClientOrderID = fill.ClientOrderID
	}
	if order.Symbol == "" {
		order.Symbol = fill.Symbol
	}
	if order.Side == "" {
		order.Side = fill.Side
	}
	if order.Timestamp == 0 {
		order.Timestamp = fill.Timestamp
	}

	prevFilled := order.FilledQuantity
	prevQuote := order.AverageFillPrice.Mul(prevFilled)
	order.LastFillPrice = fill.Price
	order.LastFillQuantity = fill.Quantity
	order.FilledQuantity = prevFilled.Add(fill.Quantity)
	if order.FilledQuantity.IsPositive() {
		totalQuote := prevQuote.Add(fill.Price.Mul(fill.Quantity))
		order.AverageFillPrice = totalQuote.Div(order.FilledQuantity)
	}
	order.Fee = order.Fee.Add(fill.Fee)

	switch {
	case order.Quantity.GreaterThan(decimal.Zero) && !order.FilledQuantity.LessThan(order.Quantity):
		order.Status = exchanges.OrderStatusFilled
	case order.FilledQuantity.GreaterThan(decimal.Zero):
		order.Status = exchanges.OrderStatusPartiallyFilled
	}

	return order
}
