package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	exchanges "github.com/QuantProcessing/exchanges"
)

type ExchangeRuntimeConfig struct {
	Name          string
	MarketType    exchanges.MarketType
	QuoteCurrency string
	Options       map[string]string
}

func BuildExchangeConfigs(cfg *Config) (ExchangeRuntimeConfig, ExchangeRuntimeConfig) {
	if cfg == nil {
		return ExchangeRuntimeConfig{}, ExchangeRuntimeConfig{}
	}
	maker := buildExchangeRuntimeConfig(cfg.MakerExchange)
	taker := buildExchangeRuntimeConfig(cfg.TakerExchange)
	return maker, taker
}

func NewExchange(ctx context.Context, rc ExchangeRuntimeConfig) (exchanges.Exchange, error) {
	ctor, err := exchanges.LookupConstructor(rc.Name)
	if err != nil {
		return nil, err
	}
	return ctor(ctx, rc.MarketType, rc.Options)
}

func NewExchangePair(ctx context.Context, cfg *Config) (exchanges.Exchange, exchanges.Exchange, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("config is required")
	}
	makerCfg, takerCfg := BuildExchangeConfigs(cfg)

	maker, err := NewExchange(ctx, makerCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create maker %s: %w", makerCfg.Name, err)
	}

	taker, err := NewExchange(ctx, takerCfg)
	if err != nil {
		_ = maker.Close()
		return nil, nil, fmt.Errorf("create taker %s: %w", takerCfg.Name, err)
	}

	return maker, taker, nil
}

func buildExchangeRuntimeConfig(name string) ExchangeRuntimeConfig {
	normalized := strings.ToUpper(strings.TrimSpace(name))
	rc := ExchangeRuntimeConfig{
		Name:          normalized,
		MarketType:    exchanges.MarketTypePerp,
		QuoteCurrency: string(exchanges.QuoteCurrencyUSDC),
		Options: map[string]string{
			"quote_currency": string(exchanges.QuoteCurrencyUSDC),
		},
	}

	switch normalized {
	case "DECIBEL":
		rc.Options["api_key"] = os.Getenv("DECIBEL_API_KEY")
		rc.Options["private_key"] = os.Getenv("DECIBEL_PRIVATE_KEY")
		rc.Options["subaccount_addr"] = os.Getenv("DECIBEL_SUBACCOUNT_ADDR")
	case "LIGHTER":
		rc.Options["private_key"] = os.Getenv("LIGHTER_PRIVATE_KEY")
		rc.Options["account_index"] = os.Getenv("LIGHTER_ACCOUNT_INDEX")
		rc.Options["key_index"] = os.Getenv("LIGHTER_KEY_INDEX")
		rc.Options["ro_token"] = os.Getenv("LIGHTER_RO_TOKEN")
	default:
		rc.Options = map[string]string{}
	}

	return rc
}
