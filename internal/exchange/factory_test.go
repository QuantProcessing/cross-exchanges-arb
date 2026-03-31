package exchange

import (
	"context"
	"testing"

	appconfig "github.com/QuantProcessing/cross-exchanges-arb/internal/config"
	exchanges "github.com/QuantProcessing/exchanges"
)

func TestBuildConfigs_DefaultsToPerpValidationProfile(t *testing.T) {
	cfg := &appconfig.Config{
		MakerExchange: "DECIBEL",
		TakerExchange: "LIGHTER",
	}

	makerCfg, takerCfg := BuildConfigs(cfg)

	if makerCfg.MarketType != exchanges.MarketTypePerp {
		t.Fatalf("maker market type = %s, want perp", makerCfg.MarketType)
	}
	if takerCfg.MarketType != exchanges.MarketTypePerp {
		t.Fatalf("taker market type = %s, want perp", takerCfg.MarketType)
	}
	if got := makerCfg.Options["quote_currency"]; got != string(exchanges.QuoteCurrencyUSDC) {
		t.Fatalf("maker quote_currency = %q, want USDC", got)
	}
	if got := takerCfg.Options["quote_currency"]; got != string(exchanges.QuoteCurrencyUSDC) {
		t.Fatalf("taker quote_currency = %q, want USDC", got)
	}
}

func TestBuildConfigs_HonorsQuoteCurrencyOverrides(t *testing.T) {
	cfg := &appconfig.Config{
		MakerExchange:      "DECIBEL",
		TakerExchange:      "LIGHTER",
		MakerQuoteCurrency: "usdt",
		TakerQuoteCurrency: "USD",
	}

	makerCfg, takerCfg := BuildConfigs(cfg)

	if got := makerCfg.Options["quote_currency"]; got != "USDT" {
		t.Fatalf("maker quote_currency = %q, want USDT", got)
	}
	if got := takerCfg.Options["quote_currency"]; got != "USD" {
		t.Fatalf("taker quote_currency = %q, want USD", got)
	}
}

func TestNewPair_RejectsUnknownExchange(t *testing.T) {
	_, _, err := NewPair(context.Background(), &appconfig.Config{
		MakerExchange: "NOPE",
		TakerExchange: "LIGHTER",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSupportedExchangeRegistrations_AvailableInBinary(t *testing.T) {
	for _, name := range []string{"EDGEX", "DECIBEL", "LIGHTER", "HYPERLIQUID"} {
		name := name
		t.Run(name, func(t *testing.T) {
			if _, err := exchanges.LookupConstructor(name); err != nil {
				t.Fatalf("LookupConstructor(%q) failed: %v", name, err)
			}
		})
	}
}

func TestBuildConfigs_UsesLegacyAndCurrentEnvNames(t *testing.T) {
	t.Setenv("EXCHANGES_EDGEX_PRIVATE_KEY", "legacy-edgex-private")
	t.Setenv("EXCHANGES_EDGEX_ACCOUNT_ID", "legacy-edgex-account")
	t.Setenv("EXCHANGES_LIGHTER_PRIVATE_KEY", "legacy-lighter-private")
	t.Setenv("EXCHANGES_LIGHTER_ACCOUNT_INDEX", "legacy-lighter-account")
	t.Setenv("EXCHANGES_LIGHTER_KEY_INDEX", "legacy-lighter-key")
	t.Setenv("EXCHANGES_LIGHTER_RO_TOKEN", "legacy-lighter-token")

	legacyMakerCfg, legacyTakerCfg := BuildConfigs(&appconfig.Config{
		MakerExchange: "EDGEX",
		TakerExchange: "LIGHTER",
	})

	if got := legacyMakerCfg.Options["private_key"]; got != "legacy-edgex-private" {
		t.Fatalf("legacy maker private_key = %q, want %q", got, "legacy-edgex-private")
	}
	if got := legacyMakerCfg.Options["account_id"]; got != "legacy-edgex-account" {
		t.Fatalf("legacy maker account_id = %q, want %q", got, "legacy-edgex-account")
	}
	if got := legacyTakerCfg.Options["private_key"]; got != "legacy-lighter-private" {
		t.Fatalf("legacy taker private_key = %q, want %q", got, "legacy-lighter-private")
	}
	if got := legacyTakerCfg.Options["account_index"]; got != "legacy-lighter-account" {
		t.Fatalf("legacy taker account_index = %q, want %q", got, "legacy-lighter-account")
	}
	if got := legacyTakerCfg.Options["key_index"]; got != "legacy-lighter-key" {
		t.Fatalf("legacy taker key_index = %q, want %q", got, "legacy-lighter-key")
	}
	if got := legacyTakerCfg.Options["ro_token"]; got != "legacy-lighter-token" {
		t.Fatalf("legacy taker ro_token = %q, want %q", got, "legacy-lighter-token")
	}

	t.Setenv("EXCHANGES_EDGEX_PRIVATE_KEY", "")
	t.Setenv("EXCHANGES_EDGEX_ACCOUNT_ID", "")
	t.Setenv("EXCHANGES_LIGHTER_PRIVATE_KEY", "")
	t.Setenv("EXCHANGES_LIGHTER_ACCOUNT_INDEX", "")
	t.Setenv("EXCHANGES_LIGHTER_KEY_INDEX", "")
	t.Setenv("EXCHANGES_LIGHTER_RO_TOKEN", "")
	t.Setenv("DECIBEL_API_KEY", "current-decibel-api")
	t.Setenv("DECIBEL_PRIVATE_KEY", "current-decibel-private")
	t.Setenv("DECIBEL_SUBACCOUNT_ADDR", "current-decibel-subaccount")
	t.Setenv("LIGHTER_PRIVATE_KEY", "current-lighter-private")
	t.Setenv("LIGHTER_ACCOUNT_INDEX", "current-lighter-account")
	t.Setenv("LIGHTER_KEY_INDEX", "current-lighter-key")
	t.Setenv("LIGHTER_RO_TOKEN", "current-lighter-token")

	currentMakerCfg, currentTakerCfg := BuildConfigs(&appconfig.Config{
		MakerExchange: "DECIBEL",
		TakerExchange: "LIGHTER",
	})

	if got := currentMakerCfg.Options["api_key"]; got != "current-decibel-api" {
		t.Fatalf("current maker api_key = %q, want %q", got, "current-decibel-api")
	}
	if got := currentMakerCfg.Options["private_key"]; got != "current-decibel-private" {
		t.Fatalf("current maker private_key = %q, want %q", got, "current-decibel-private")
	}
	if got := currentMakerCfg.Options["subaccount_addr"]; got != "current-decibel-subaccount" {
		t.Fatalf("current maker subaccount_addr = %q, want %q", got, "current-decibel-subaccount")
	}
	if got := currentTakerCfg.Options["private_key"]; got != "current-lighter-private" {
		t.Fatalf("current taker private_key = %q, want %q", got, "current-lighter-private")
	}
	if got := currentTakerCfg.Options["account_index"]; got != "current-lighter-account" {
		t.Fatalf("current taker account_index = %q, want %q", got, "current-lighter-account")
	}
	if got := currentTakerCfg.Options["key_index"]; got != "current-lighter-key" {
		t.Fatalf("current taker key_index = %q, want %q", got, "current-lighter-key")
	}
	if got := currentTakerCfg.Options["ro_token"]; got != "current-lighter-token" {
		t.Fatalf("current taker ro_token = %q, want %q", got, "current-lighter-token")
	}
}
