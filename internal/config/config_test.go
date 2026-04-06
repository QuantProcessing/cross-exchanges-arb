package config

import (
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestParse_DefaultMakerAndTaker(t *testing.T) {
	origArgs := os.Args
	origCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origCommandLine
	})

	os.Args = []string{"cross-exchanges-arb"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

	cfg := Parse()
	if cfg.MakerExchange != "EDGEX" {
		t.Fatalf("default maker = %q, want EDGEX", cfg.MakerExchange)
	}
	if cfg.TakerExchange != "LIGHTER" {
		t.Fatalf("default taker = %q, want LIGHTER", cfg.TakerExchange)
	}
}

func TestConfigString_ShowsValidationKnobsWithoutClaimingValidationMode(t *testing.T) {
	cfg := &Config{
		MakerExchange: "DECIBEL",
		TakerExchange: "LIGHTER",
		Symbol:        "BTC",
		Quantity:      decimal.RequireFromString("0.001"),
		MakerTimeout:  20 * time.Second,
		MaxRounds:     2,
	}

	summary := cfg.String()
	if strings.Contains(summary, "Mode:") {
		t.Fatal("startup summary must not expose runtime mode selection")
	}
	if !strings.Contains(summary, "MakerTimeout: 20s") {
		t.Fatal("startup summary should show maker timeout")
	}
	if !strings.Contains(summary, "MaxRounds: 2") {
		t.Fatal("startup summary should show max rounds")
	}
}
