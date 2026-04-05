package trading

import (
	"fmt"
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
)

type latencyReport struct {
	SubmitToExchange          time.Duration
	ExchangeToLocalAck        time.Duration
	ExchangeToLocalFill       time.Duration
	SubmitToFirstFillExchange time.Duration
	SubmitToFirstFillLocal    time.Duration
}

type orderTrace struct {
	RoundID        int
	Phase          string
	Exchange       string
	SubmitLocal    time.Time
	AckLocal       time.Time
	OrderTimestamp int64
	FirstFillLocal time.Time
}

func buildLatencyReport(submitLocal, ackLocal time.Time, order *exchanges.Order, firstFillLocal time.Time, fill *exchanges.Fill) latencyReport {
	var report latencyReport
	orderTS := timestampMillisToTime(0)
	if order != nil {
		orderTS = timestampMillisToTime(order.Timestamp)
	}
	fillTS := timestampMillisToTime(0)
	if fill != nil {
		fillTS = timestampMillisToTime(fill.Timestamp)
	}

	if !submitLocal.IsZero() && !orderTS.IsZero() {
		report.SubmitToExchange = orderTS.Sub(submitLocal)
	}
	if !ackLocal.IsZero() && !orderTS.IsZero() {
		report.ExchangeToLocalAck = ackLocal.Sub(orderTS)
	}
	if !submitLocal.IsZero() && !firstFillLocal.IsZero() {
		report.SubmitToFirstFillLocal = firstFillLocal.Sub(submitLocal)
	}
	if !submitLocal.IsZero() && !fillTS.IsZero() {
		report.SubmitToFirstFillExchange = fillTS.Sub(submitLocal)
	}
	if !fillTS.IsZero() && !firstFillLocal.IsZero() {
		report.ExchangeToLocalFill = firstFillLocal.Sub(fillTS)
	}

	return report
}

func (t *Trader) registerOrderTrace(phase, exchange string, order *exchanges.Order, submitLocal, ackLocal time.Time) {
	if t == nil || order == nil {
		return
	}

	trace := &orderTrace{
		RoundID:        t.roundID,
		Phase:          phase,
		Exchange:       exchange,
		SubmitLocal:    submitLocal,
		AckLocal:       ackLocal,
		OrderTimestamp: order.Timestamp,
	}

	t.mu.Lock()
	if t.orderTraces == nil {
		t.orderTraces = make(map[string]*orderTrace)
	}
	storeOrderTraceLocked(t.orderTraces, exchange, order.OrderID, order.ClientOrderID, trace)
	t.mu.Unlock()

	report := buildLatencyReport(submitLocal, ackLocal, order, time.Time{}, nil)
	t.logger.Infof("%s 🧾 %s %s ack order=%s client=%s status=%s order_px=%s qty=%s filled=%s exchange_ts=%s latency[submit->exchange=%s exchange->local_ack=%s]",
		formatRoundTag(trace.RoundID),
		phase,
		exchange,
		order.OrderID,
		order.ClientOrderID,
		order.Status,
		orderPriceString(order),
		order.Quantity,
		order.FilledQuantity,
		timestampString(order.Timestamp),
		latencyString(report.SubmitToExchange, order.Timestamp > 0),
		latencyString(report.ExchangeToLocalAck, order.Timestamp > 0),
	)
}

func (t *Trader) observeOrderUpdate(exchange string, order *exchanges.Order) {
	if t == nil || order == nil {
		return
	}

	now := time.Now()
	t.mu.Lock()
	trace := findOrderTraceLocked(t.orderTraces, exchange, order.OrderID, order.ClientOrderID)
	if trace != nil && order.Timestamp > 0 {
		trace.OrderTimestamp = order.Timestamp
	}
	t.mu.Unlock()
	if trace == nil {
		return
	}

	t.logger.Infof("%s 📦 %s %s update order=%s status=%s filled=%s avg_fill=%s last_fill=%s last_fill_qty=%s exchange_ts=%s latency[exchange->local_update=%s]",
		formatRoundTag(trace.RoundID),
		trace.Phase,
		exchange,
		order.OrderID,
		order.Status,
		order.FilledQuantity,
		order.AverageFillPrice,
		order.LastFillPrice,
		order.LastFillQuantity,
		timestampString(order.Timestamp),
		latencyString(now.Sub(timestampMillisToTime(order.Timestamp)), order.Timestamp > 0),
	)
}

func (t *Trader) observeFill(exchange string, fill *exchanges.Fill) {
	if t == nil || fill == nil {
		return
	}

	now := time.Now()
	t.mu.Lock()
	trace := findOrderTraceLocked(t.orderTraces, exchange, fill.OrderID, fill.ClientOrderID)
	var (
		submitLocal time.Time
		ackLocal    time.Time
		orderTS     int64
		phase       string
		roundID     int
		first       bool
	)
	if trace != nil {
		submitLocal = trace.SubmitLocal
		ackLocal = trace.AckLocal
		orderTS = trace.OrderTimestamp
		phase = trace.Phase
		roundID = trace.RoundID
		if trace.FirstFillLocal.IsZero() {
			trace.FirstFillLocal = now
			first = true
		}
	}
	t.mu.Unlock()
	if trace == nil {
		return
	}

	report := buildLatencyReport(submitLocal, ackLocal, &exchanges.Order{Timestamp: orderTS}, now, fill)
	t.logger.Infof("%s 💱 %s %s fill trade=%s order=%s qty=%s price=%s fee=%s fee_asset=%s maker=%t exchange_ts=%s latency[exchange->local_fill=%s%s%s]",
		formatRoundTag(roundID),
		phase,
		exchange,
		fill.TradeID,
		fill.OrderID,
		fill.Quantity,
		fill.Price,
		fill.Fee,
		fill.FeeAsset,
		fill.IsMaker,
		timestampString(fill.Timestamp),
		latencyString(report.ExchangeToLocalFill, fill.Timestamp > 0),
		firstFillSegment(" submit->first_fill_exchange", report.SubmitToFirstFillExchange, fill.Timestamp > 0 && !submitLocal.IsZero(), first),
		firstFillSegment(" submit->first_fill_local", report.SubmitToFirstFillLocal, !submitLocal.IsZero(), first),
	)
}

func firstFillSegment(label string, value time.Duration, valid, first bool) string {
	if !first {
		return ""
	}
	return fmt.Sprintf(" %s=%s", label, latencyString(value, valid))
}

func storeOrderTraceLocked(store map[string]*orderTrace, exchange, orderID, clientOrderID string, trace *orderTrace) {
	if orderID != "" {
		store[orderTraceKey(exchange, "order", orderID)] = trace
	}
	if clientOrderID != "" {
		store[orderTraceKey(exchange, "client", clientOrderID)] = trace
	}
}

func findOrderTraceLocked(store map[string]*orderTrace, exchange, orderID, clientOrderID string) *orderTrace {
	if store == nil {
		return nil
	}
	if orderID != "" {
		if trace := store[orderTraceKey(exchange, "order", orderID)]; trace != nil {
			return trace
		}
	}
	if clientOrderID != "" {
		if trace := store[orderTraceKey(exchange, "client", clientOrderID)]; trace != nil {
			return trace
		}
	}
	return nil
}

func orderTraceKey(exchange, kind, id string) string {
	return fmt.Sprintf("%s|%s|%s", exchange, kind, id)
}

func formatRoundTag(id int) string {
	return fmt.Sprintf("R%03d", id)
}

func orderPriceString(order *exchanges.Order) string {
	if order == nil {
		return "0"
	}
	if !order.OrderPrice.IsZero() {
		return order.OrderPrice.String()
	}
	if order.Price.IsZero() {
		return "0"
	}
	return order.Price.String()
}

func latencyString(value time.Duration, valid bool) string {
	if !valid {
		return "n/a"
	}
	return fmt.Sprintf("%dms", value.Milliseconds())
}

func timestampString(ts int64) string {
	tm := timestampMillisToTime(ts)
	if tm.IsZero() {
		return "n/a"
	}
	return tm.Format(time.RFC3339Nano)
}

func timestampMillisToTime(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	if ts > 1_000_000_000_000 {
		return time.UnixMilli(ts).UTC()
	}
	return time.Unix(ts, 0).UTC()
}
