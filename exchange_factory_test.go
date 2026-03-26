package main

import (
	"context"
	"testing"

	exchanges "github.com/QuantProcessing/exchanges"
)

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
