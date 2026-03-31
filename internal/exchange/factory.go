package exchange

import (
	"context"
	"fmt"
	"os"
	"strings"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	exchanges "github.com/QuantProcessing/exchanges"
)

type RuntimeConfig struct {
	Name       string
	MarketType exchanges.MarketType
	Options    map[string]string
}

func BuildConfigs(cfg *appconfig.Config) (RuntimeConfig, RuntimeConfig) {
	if cfg == nil {
		return RuntimeConfig{}, RuntimeConfig{}
	}
	maker := buildExchangeRuntimeConfig(cfg.MakerExchange, cfg.MakerQuoteCurrency)
	taker := buildExchangeRuntimeConfig(cfg.TakerExchange, cfg.TakerQuoteCurrency)
	return maker, taker
}

func New(ctx context.Context, rc RuntimeConfig) (exchanges.Exchange, error) {
	ctor, err := exchanges.LookupConstructor(rc.Name)
	if err != nil {
		return nil, err
	}
	return ctor(ctx, rc.MarketType, rc.Options)
}

func NewPair(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("config is required")
	}
	makerCfg, takerCfg := BuildConfigs(cfg)

	maker, err := New(ctx, makerCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create maker %s: %w", makerCfg.Name, err)
	}

	taker, err := New(ctx, takerCfg)
	if err != nil {
		_ = maker.Close()
		return nil, nil, fmt.Errorf("create taker %s: %w", takerCfg.Name, err)
	}

	return maker, taker, nil
}

func buildExchangeRuntimeConfig(name, quoteCurrency string) RuntimeConfig {
	normalized := strings.ToUpper(strings.TrimSpace(name))
	rc := RuntimeConfig{
		Name:       normalized,
		MarketType: exchanges.MarketTypePerp,
		Options: map[string]string{
			"quote_currency": string(exchanges.QuoteCurrencyUSDC),
		},
	}
	if normalizedQuoteCurrency := strings.ToUpper(strings.TrimSpace(quoteCurrency)); normalizedQuoteCurrency != "" {
		rc.Options["quote_currency"] = normalizedQuoteCurrency
	}

	switch normalized {
	case "EDGEX":
		rc.Options["private_key"] = envFirst(
			"EXCHANGES_EDGEX_PRIVATE_KEY",
			"EDGEX_PRIVATE_KEY",
		)
		rc.Options["account_id"] = envFirst(
			"EXCHANGES_EDGEX_ACCOUNT_ID",
			"EDGEX_ACCOUNT_ID",
		)
	case "DECIBEL":
		rc.Options["api_key"] = envFirst(
			"DECIBEL_API_KEY",
			"EXCHANGES_DECIBEL_API_KEY",
		)
		rc.Options["private_key"] = envFirst(
			"DECIBEL_PRIVATE_KEY",
			"EXCHANGES_DECIBEL_PRIVATE_KEY",
		)
		rc.Options["subaccount_addr"] = envFirst(
			"DECIBEL_SUBACCOUNT_ADDR",
			"EXCHANGES_DECIBEL_SUBACCOUNT_ADDR",
		)
	case "LIGHTER":
		rc.Options["private_key"] = envFirst(
			"EXCHANGES_LIGHTER_PRIVATE_KEY",
			"LIGHTER_PRIVATE_KEY",
		)
		rc.Options["account_index"] = envFirst(
			"EXCHANGES_LIGHTER_ACCOUNT_INDEX",
			"LIGHTER_ACCOUNT_INDEX",
		)
		rc.Options["key_index"] = envFirst(
			"EXCHANGES_LIGHTER_KEY_INDEX",
			"LIGHTER_KEY_INDEX",
		)
		rc.Options["ro_token"] = envFirst(
			"EXCHANGES_LIGHTER_RO_TOKEN",
			"LIGHTER_RO_TOKEN",
		)
	case "HYPERLIQUID":
		rc.Options["private_key"] = envFirst(
			"EXCHANGES_HYPERLIQUID_PRIVATE_KEY",
			"HYPERLIQUID_PRIVATE_KEY",
		)
		rc.Options["account_addr"] = envFirst(
			"EXCHANGES_HYPERLIQUID_ACCOUNT_ADDR",
			"HYPERLIQUID_ACCOUNT_ADDR",
		)
	default:
		// Keep existing options (e.g. quote_currency) for unknown exchanges.
	}

	return rc
}

func envFirst(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}
