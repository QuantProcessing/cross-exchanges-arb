package runlog

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

type Event struct {
	At time.Time `json:"at"`

	Category string `json:"category"`
	Type     string `json:"type"`

	MakerExchange string `json:"maker_exchange,omitempty"`
	TakerExchange string `json:"taker_exchange,omitempty"`
	Symbol        string `json:"symbol,omitempty"`

	Round     int    `json:"round,omitempty"`
	State     string `json:"state,omitempty"`
	Direction string `json:"direction,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Result    string `json:"result,omitempty"`
	Detail    string `json:"detail,omitempty"`

	Side     string `json:"side,omitempty"`
	Exchange string `json:"exchange,omitempty"`
	Price    string `json:"price,omitempty"`
	Quantity string `json:"qty,omitempty"`

	SpreadBps         float64 `json:"spread_bps,omitempty"`
	ZScore            float64 `json:"z,omitempty"`
	ExpectedProfitBps float64 `json:"expected_profit_bps,omitempty"`
	WaitMS            int64   `json:"wait_ms,omitempty"`
	HeldMS            int64   `json:"held_ms,omitempty"`
	Rounds            int     `json:"rounds,omitempty"`

	Safe       string `json:"safe,omitempty"`
	Profit     string `json:"profit,omitempty"`
	Blocked    string `json:"blocked,omitempty"`
	Market     string `json:"market,omitempty"`
	MakerLagMS int64  `json:"maker_lag_ms,omitempty"`
	TakerLagMS int64  `json:"taker_lag_ms,omitempty"`

	MakerBalance string `json:"maker_balance,omitempty"`
	TakerBalance string `json:"taker_balance,omitempty"`
	MakerPnL     string `json:"maker_pnl,omitempty"`
	TakerPnL     string `json:"taker_pnl,omitempty"`
	TotalPnL     string `json:"total_pnl,omitempty"`
}

type EventSink struct {
	mu     sync.Mutex
	enc    *json.Encoder
	closer io.Closer
}

func NewEventSink(w io.WriteCloser) *EventSink {
	if w == nil {
		return nil
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	return &EventSink{
		enc:    enc,
		closer: w,
	}
}

func (s *EventSink) Record(event Event) error {
	if s == nil || s.enc == nil {
		return nil
	}

	if event.At.IsZero() {
		event.At = time.Now().UTC()
	} else {
		event.At = event.At.UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.enc.Encode(event)
}

func (s *EventSink) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.closer.Close()
	s.closer = nil
	return err
}
