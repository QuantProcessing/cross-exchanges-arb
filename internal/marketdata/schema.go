package marketdata

import (
	"time"

	"github.com/shopspring/decimal"
)

type Side string

const (
	SideMaker Side = "maker"
	SideTaker Side = "taker"
)

type MarketLevel struct {
	Price    decimal.Decimal `json:"price"`
	Quantity decimal.Decimal `json:"qty"`
}

type MarketBook struct {
	Bids []MarketLevel `json:"bids"`
	Asks []MarketLevel `json:"asks"`
}

type MarketFrame struct {
	SchemaVersion   int        `json:"schema_version"`
	LocalTime       time.Time  `json:"ts_local"`
	UpdatedSide     Side       `json:"updated_side"`
	Symbol          string     `json:"symbol"`
	MakerExchange   string     `json:"maker_exchange"`
	TakerExchange   string     `json:"taker_exchange"`
	MakerExchangeTS time.Time  `json:"maker_ts_exchange,omitempty"`
	TakerExchangeTS time.Time  `json:"taker_ts_exchange,omitempty"`
	MakerBook       MarketBook `json:"maker_book"`
	TakerBook       MarketBook `json:"taker_book"`
}

type RawRecord struct {
	LocalTime  time.Time   `json:"ts_local"`
	Side       Side        `json:"side"`
	Exchange   string      `json:"exchange"`
	Symbol     string      `json:"symbol"`
	ExchangeTS time.Time   `json:"exchange_ts,omitempty"`
	QuoteLagMS int64       `json:"quote_lag_ms"`
	Bids       []MarketLevel `json:"bids"`
	Asks       []MarketLevel `json:"asks"`
}
