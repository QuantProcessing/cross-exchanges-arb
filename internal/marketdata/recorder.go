package marketdata

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

type Recorder struct {
	makerExchange string
	takerExchange string
	symbol        string
	w             io.Writer
	mu            sync.Mutex
}

func NewRecorder(makerExchange, takerExchange, symbol string, w io.Writer) *Recorder {
	return &Recorder{
		makerExchange: makerExchange,
		takerExchange: takerExchange,
		symbol:        symbol,
		w:             w,
	}
}

func (r *Recorder) Record(frame MarketFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if frame.MakerExchange == "" {
		frame.MakerExchange = r.makerExchange
	}
	if frame.TakerExchange == "" {
		frame.TakerExchange = r.takerExchange
	}
	if frame.Symbol == "" {
		frame.Symbol = r.symbol
	}

	enc := json.NewEncoder(r.w)
	if err := enc.Encode(rawRecord(frame, SideMaker)); err != nil {
		return err
	}
	return enc.Encode(rawRecord(frame, SideTaker))
}

func rawRecord(frame MarketFrame, side Side) RawRecord {
	record := RawRecord{
		LocalTime: frame.LocalTime,
		Side:      side,
		Symbol:    frame.Symbol,
	}

	switch side {
	case SideMaker:
		record.Exchange = frame.MakerExchange
		record.ExchangeTS = frame.MakerExchangeTS
		record.QuoteLagMS = rawQuoteLagMS(frame.LocalTime, frame.MakerExchangeTS)
		record.Bids = frame.MakerBook.Bids
		record.Asks = frame.MakerBook.Asks
	case SideTaker:
		record.Exchange = frame.TakerExchange
		record.ExchangeTS = frame.TakerExchangeTS
		record.QuoteLagMS = rawQuoteLagMS(frame.LocalTime, frame.TakerExchangeTS)
		record.Bids = frame.TakerBook.Bids
		record.Asks = frame.TakerBook.Asks
	}

	return record
}

func rawQuoteLagMS(local, exchange time.Time) int64 {
	if local.IsZero() || exchange.IsZero() {
		return 0
	}
	return local.Sub(exchange).Milliseconds()
}
