package main

import (
	"context"
	"flag"
	"os"
	"testing"

	exchanges "github.com/QuantProcessing/exchanges"
)

func TestDefaultExecutionProfile_UsesValidationDefaults(t *testing.T) {
	p := DefaultExecutionProfile()
	if !p.LiveValidation {
		t.Fatal("expected live validation enabled by default for validation profile")
	}
	if p.EntryMakerOrderType != exchanges.OrderTypePostOnly {
		t.Fatalf("entry type = %s, want post-only", p.EntryMakerOrderType)
	}
}

func TestBuildExchangeConfigs_DefaultsToPerpValidationProfile(t *testing.T) {
	cfg := &Config{
		MakerExchange: "DECIBEL",
		TakerExchange: "LIGHTER",
	}

	makerCfg, takerCfg := BuildExchangeConfigs(cfg)

	if makerCfg.MarketType != exchanges.MarketTypePerp {
		t.Fatalf("maker market type = %s, want perp", makerCfg.MarketType)
	}
	if takerCfg.MarketType != exchanges.MarketTypePerp {
		t.Fatalf("taker market type = %s, want perp", takerCfg.MarketType)
	}
}

func TestNewExchangePair_RejectsUnknownExchange(t *testing.T) {
	_, _, err := NewExchangePair(context.Background(), &Config{
		MakerExchange: "NOPE",
		TakerExchange: "LIGHTER",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSupportedExchangeRegistrations_AvailableInBinary(t *testing.T) {
	for _, name := range []string{"EDGEX", "DECIBEL", "LIGHTER"} {
		name := name
		t.Run(name, func(t *testing.T) {
			if _, err := exchanges.LookupConstructor(name); err != nil {
				t.Fatalf("LookupConstructor(%q) failed: %v", name, err)
			}
		})
	}
}

func TestBuildExchangeConfigs_UsesLegacyAndCurrentEnvNames(t *testing.T) {
	t.Setenv("EXCHANGES_EDGEX_PRIVATE_KEY", "legacy-edgex-private")
	t.Setenv("EXCHANGES_EDGEX_ACCOUNT_ID", "legacy-edgex-account")
	t.Setenv("EXCHANGES_LIGHTER_PRIVATE_KEY", "legacy-lighter-private")
	t.Setenv("EXCHANGES_LIGHTER_ACCOUNT_INDEX", "legacy-lighter-account")
	t.Setenv("EXCHANGES_LIGHTER_KEY_INDEX", "legacy-lighter-key")
	t.Setenv("EXCHANGES_LIGHTER_RO_TOKEN", "legacy-lighter-token")

	legacyMakerCfg, legacyTakerCfg := BuildExchangeConfigs(&Config{
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

	currentMakerCfg, currentTakerCfg := BuildExchangeConfigs(&Config{
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

func TestParseConfig_DefaultMakerAndTaker(t *testing.T) {
	origArgs := os.Args
	origCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origCommandLine
	})

	os.Args = []string{"cross-exchanges-arb"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

	cfg := ParseConfig()

	if cfg.MakerExchange != "EDGEX" {
		t.Fatalf("default maker = %q, want EDGEX", cfg.MakerExchange)
	}
	if cfg.TakerExchange != "LIGHTER" {
		t.Fatalf("default taker = %q, want LIGHTER", cfg.TakerExchange)
	}
}
