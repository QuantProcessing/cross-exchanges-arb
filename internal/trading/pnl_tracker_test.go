package trading

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantProcessing/cross-exchanges-arb/internal/runlog"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPnLTrackerLogsEVTPnlRefreshAndRealized(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)

	maker := newTestExchange("maker")
	taker := newTestExchange("taker")
	maker.balance = decimal.RequireFromString("1000")
	taker.balance = decimal.RequireFromString("2000")

	tracker := NewPnLTracker(context.Background(), maker, taker, "maker", "taker", zap.New(core).Sugar())
	eventsFile, err := os.Create(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("Create(events.jsonl): %v", err)
	}
	sink := runlog.NewEventSink(eventsFile)
	t.Cleanup(func() { _ = sink.Close() })
	tracker.SetEventSink(sink)

	maker.balance = decimal.RequireFromString("1005")
	taker.balance = decimal.RequireFromString("1998")
	tracker.lastRefresh = time.Now().Add(-6 * time.Minute)
	tracker.PeriodicRefresh(context.Background())

	if observed.FilterMessageSnippet("EVT pnl_refresh").Len() != 1 {
		t.Fatalf("pnl_refresh event logs = %d, want 1", observed.FilterMessageSnippet("EVT pnl_refresh").Len())
	}

	maker.balance = decimal.RequireFromString("1007")
	taker.balance = decimal.RequireFromString("2001")
	tracker.OnRoundComplete(context.Background())

	if observed.FilterMessageSnippet("EVT pnl_realized").Len() != 1 {
		t.Fatalf("pnl_realized event logs = %d, want 1", observed.FilterMessageSnippet("EVT pnl_realized").Len())
	}

	data, err := os.ReadFile(eventsFile.Name())
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", eventsFile.Name(), err)
	}
	if got := strings.Count(string(data), `"category":"pnl"`); got != 2 {
		t.Fatalf("pnl event count = %d, want 2; data=%s", got, data)
	}
}
