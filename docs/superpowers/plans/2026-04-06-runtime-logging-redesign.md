# Runtime Logging Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `run.log` into an operator-first stream that answers safety, profitability, and blockage quickly, while moving replay-oriented data into a small event-based structured log and keeping `raw.jsonl` as the raw market evidence stream.

**Architecture:** Keep the redesign intentionally small. Reuse the existing run artifact session, trader state machine, and raw market recorder. Replace per-frame `MKT` log spam with a heartbeat summary plus concise lifecycle events, then add a minimal event sink for `session`, `round`, `pnl`, and `health` records. Delete or demote noisy logging instead of introducing a new telemetry subsystem.

**Tech Stack:** Go 1.26, Zap, existing `internal/app`, `internal/runlog`, `internal/trading`, `internal/marketdata`, and `internal/spread` packages

---

## File Map

### Existing files to modify

- `internal/app/run.go`
- `internal/app/run_test.go`
- `internal/runlog/session.go`
- `internal/trading/trader.go`
- `internal/trading/open.go`
- `internal/trading/close.go`
- `internal/trading/pnl_tracker.go`
- `internal/trading/trader_test.go`
- `README.md`
- `README_CN.md`

### New files to create

- `internal/runlog/events.go`
- `internal/runlog/events_test.go`
- `internal/trading/status.go`
- `internal/trading/status_test.go`

## Task 1: Lock the New Operator Contract with Tests

**Files:**
- Modify: `internal/app/run_test.go`
- Create: `internal/trading/status_test.go`
- Modify: `internal/trading/trader_test.go`

- [ ] **Step 1: Write a failing app-level test that proves `run.log` no longer emits per-frame `MKT` lines**

```go
func TestRun_LogOmitsPerFrameMarketDump(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()

	book := &exchanges.OrderBook{
		Symbol:    "BTC",
		Timestamp: 1710000000000,
		Bids: []exchanges.Level{{Price: decimal.RequireFromString("100"), Quantity: decimal.RequireFromString("1")}},
		Asks: []exchanges.Level{{Price: decimal.RequireFromString("100.1"), Quantity: decimal.RequireFromString("1")}},
	}
	maker := &testExchange{localBook: book, emitInitialBook: true}
	taker := &testExchange{localBook: book, emitInitialBook: true}

	oldNewExchangePair := newExchangePair
	t.Cleanup(func() { newExchangePair = oldNewExchangePair })
	newExchangePair = func(ctx context.Context, cfg *appconfig.Config) (exchanges.Exchange, exchanges.Exchange, error) {
		return maker, taker, nil
	}

	withTempWorkingDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()

	cfg := &appconfig.Config{
		MakerExchange:  "TEST",
		TakerExchange:  "TEST",
		Symbol:         "BTC",
		Quantity:       decimal.RequireFromString("0.001"),
		WindowSize:     10,
		WarmupTicks:    1,
		WarmupDuration: 50 * time.Millisecond,
	}

	_ = Run(ctx, cfg, logger)

	if logs.FilterMessageSnippet("MKT ").Len() != 0 {
		t.Fatalf("unexpected per-frame market dump in operator log")
	}
}
```

- [ ] **Step 2: Write failing unit tests for the operator status verdicts**

```go
func TestBuildOperatorStatus_MapsSafeProfitAndBlocked(t *testing.T) {
	tests := []struct {
		name string
		tr   *Trader
		want operatorStatus
	}{
		{
			name: "idle safe",
			tr: &Trader{
				state:           StateIdle,
				completedRounds: 3,
			},
			want: operatorStatus{
				Safe:    SafeLevelSafe,
				Blocked: "none",
			},
		},
		{
			name: "manual intervention is danger",
			tr: &Trader{
				state: StateManualIntervention,
			},
			want: operatorStatus{
				Safe:    SafeLevelDanger,
				Blocked: "manual_intervention",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tr.buildOperatorStatus(time.Now())
			if got.Safe != tc.want.Safe || got.Blocked != tc.want.Blocked {
				t.Fatalf("status = %#v, want %#v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run the targeted tests and verify they fail for the right reasons**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/app ./internal/trading -run 'TestRun_LogOmitsPerFrameMarketDump|TestBuildOperatorStatus_MapsSafeProfitAndBlocked' -count=1
```

Expected:

- `TestRun_LogOmitsPerFrameMarketDump` fails because `MKT` lines are still emitted
- `TestBuildOperatorStatus_MapsSafeProfitAndBlocked` fails because the status builder does not exist yet

- [ ] **Step 4: Add one regression test that asserts lifecycle logs still surface the key round transitions**

```go
func TestTraderLifecycleLogsRemainVisible(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	tr := newTestTraderWithLogger(zap.New(core).Sugar())

	tr.logger.Infof("EVT signal round=1 dir=%s edge=+10.0bps qty=0.01", spread.LongMakerShortTaker)
	tr.logger.Infof("EVT maker_placed round=1 px=100 qty=0.01")
	tr.logger.Infof("EVT close_done round=1 reason=target_reached net=+8.0bps total=+8.0bps")

	if observed.FilterMessageSnippet("EVT signal").Len() != 1 {
		t.Fatalf("missing signal event")
	}
	if observed.FilterMessageSnippet("EVT close_done").Len() != 1 {
		t.Fatalf("missing close_done event")
	}
}
```

- [ ] **Step 5: Re-run the targeted tests after implementation and verify they pass before moving on**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/app ./internal/trading -run 'TestRun_LogOmitsPerFrameMarketDump|TestBuildOperatorStatus_MapsSafeProfitAndBlocked|TestTraderLifecycleLogsRemainVisible' -count=1
```

Expected:

- all three tests pass

## Task 2: Replace Per-Frame Market Dumps with a Heartbeat Aggregator

**Files:**
- Modify: `internal/app/run.go`
- Create: `internal/trading/status.go`
- Create: `internal/trading/status_test.go`
- Modify: `internal/trading/trader.go`

- [ ] **Step 1: Add a status snapshot type that can produce `safe`, `profit`, and `blocked` without touching execution logic**

```go
type SafeLevel string

const (
	SafeLevelSafe   SafeLevel = "SAFE"
	SafeLevelWarn   SafeLevel = "WARN"
	SafeLevelDanger SafeLevel = "DANGER"
)

type operatorStatus struct {
	Safe         SafeLevel
	Profit       string
	State        State
	Blocked      string
	MakerLagMS   int64
	TakerLagMS   int64
	Rounds       int
	MakerOrderAge time.Duration
	PositionHold  time.Duration
}
```

- [ ] **Step 2: Extend `Trader` with a read-only status method and enough state to support heartbeats**

```go
type Trader struct {
	// existing fields...
	lastMakerFillAt  time.Time
	lastHedgeDoneAt  time.Time
	lastCloseReason  string
	lastRoundNetBps  float64
	lastTotalNetBps  float64
}

func (t *Trader) buildOperatorStatus(now time.Time) operatorStatus {
	t.mu.Lock()
	defer t.mu.Unlock()

	status := operatorStatus{
		State:  t.state,
		Rounds: t.completedRounds,
	}
	// Derive Safe, Profit, and Blocked from current state and tracked timestamps.
	return status
}
```

- [ ] **Step 3: Replace the current `logMarketSnapshot` subscription with a ticker-driven heartbeat**

```go
heartbeat := time.NewTicker(2 * time.Second)
defer heartbeat.Stop()

go func() {
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			status := trader.BuildOperatorStatus(time.Now(), marketService.LastFrame())
			logger.Infof(
				"STAT safe=%s profit=%s state=%s blocked=%s maker_lag=%dms taker_lag=%dms rounds=%d",
				status.Safe,
				status.Profit,
				status.State,
				status.Blocked,
				status.MakerLagMS,
				status.TakerLagMS,
				status.Rounds,
			)
		}
	}
}()
```

- [ ] **Step 4: Delete or inline-remove the old market dump helpers once the heartbeat is in place**

```go
// Remove:
// - type marketStats
// - metricsStatsFromSnapshot
// - quoteLagMS
// - zScore
// - roundBps
// - logMarketSnapshot
```

- [ ] **Step 5: Run the focused verification for heartbeat behavior and status verdicts**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/app ./internal/trading -run 'TestRun_LogOmitsPerFrameMarketDump|TestBuildOperatorStatus_MapsSafeProfitAndBlocked' -count=1
```

Expected:

- no `MKT` logs
- heartbeat/status logic covered by passing tests

## Task 3: Normalize Trading Logs into Concise `EVT` Messages

**Files:**
- Modify: `internal/trading/open.go`
- Modify: `internal/trading/close.go`
- Modify: `internal/trading/pnl_tracker.go`
- Modify: `internal/trading/trader.go`
- Modify: `internal/trading/trader_test.go`

- [ ] **Step 1: Write failing tests for the new event style on the most important lifecycle points**

```go
func TestHandleSignal_LogsOperatorEvent(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	tr := newTestTraderWithLogger(zap.New(core).Sugar())

	tr.HandleSignal(&spread.Signal{
		Direction:      spread.LongMakerShortTaker,
		SpreadBps:      12.3,
		ZScore:         2.4,
		ExpectedProfit: 8.7,
		Quantity:       decimal.RequireFromString("0.01"),
	})

	if observed.FilterMessageSnippet("EVT signal").Len() != 1 {
		t.Fatalf("missing EVT signal log")
	}
}
```

- [ ] **Step 2: Convert signal, maker, hedge, close, and manual-intervention logs to a shared concise style**

```go
t.logger.Infof("%s EVT signal dir=%s edge=%+.1fbps qty=%s", roundTag, sig.Direction, sig.SpreadBps, qty)
t.logger.Infof("%s EVT maker_placed px=%s qty=%s wait=%dms", t.roundTag(), makerPrice, qty, makerPlacedAt.Sub(signalTime).Milliseconds())
t.logger.Infof("%s EVT hedge_done wait=%dms", t.roundTag(), hedgeDone.Sub(hedgeStart).Milliseconds())
t.logger.Infof("%s EVT close_done reason=%s net=%+.1fbps total=%+.1fbps", t.roundTag(), reason, metrics.NetBps, t.lastTotalNetBps)
t.logger.Errorf("%s EVT manual_intervention reason=%q", t.roundTag(), errMsg)
```

- [ ] **Step 3: Keep detailed evidence, but only emit it when a round closes or fails**

```go
t.logger.Infof("%s EVT round_recap reason=%q hold=%s %s",
	t.roundTag(),
	reason,
	time.Since(pos.OpenTime).Round(time.Millisecond),
	pos.OpenRecap.Summary(),
)
```

- [ ] **Step 4: Update PnL logging so the operator message stays compact and the tracker exposes totals for the heartbeat**

```go
func (p *PnLTracker) TotalNetSummary() string {
	return fmt.Sprintf("%+sbps", p.totalNetBps)
}

p.logger.Infof("EVT pnl_refresh total=%s rounds=%d", totalPnL, p.rounds)
```

- [ ] **Step 5: Run targeted trader verification and inspect the emitted message set**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'TestHandleSignal_LogsOperatorEvent|TestTraderLifecycleLogsRemainVisible' -count=1
```

Expected:

- `EVT signal`, `EVT maker_placed`, and `EVT close_done` are present
- no test depends on the old emoji-heavy message format

## Task 4: Add a Minimal Structured Event Sink

**Files:**
- Create: `internal/runlog/events.go`
- Create: `internal/runlog/events_test.go`
- Modify: `internal/runlog/session.go`
- Modify: `internal/app/run.go`
- Modify: `internal/trading/open.go`
- Modify: `internal/trading/close.go`
- Modify: `internal/trading/pnl_tracker.go`

- [ ] **Step 1: Write a failing test for JSONL event encoding with a flat, event-based schema**

```go
func TestEventRecorder_WritesRoundEventJSONL(t *testing.T) {
	var buf bytes.Buffer
	rec := NewEventRecorder(&buf)

	err := rec.Record(Event{
		TS:      time.Unix(1710000000, 0).UTC(),
		Kind:    "round",
		Event:   "close_done",
		RoundID: 7,
		State:   "cooldown",
		Reason:  "target_reached",
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	if !strings.Contains(buf.String(), "\"event\":\"close_done\"") {
		t.Fatalf("missing close_done event in jsonl output: %s", buf.String())
	}
}
```

- [ ] **Step 2: Add a small recorder that writes `events.jsonl` without introducing nested schemas**

```go
type Event struct {
	TS          time.Time `json:"ts"`
	Kind        string    `json:"kind"`
	Event       string    `json:"event"`
	RoundID     int       `json:"round_id,omitempty"`
	State       string    `json:"state,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Direction   string    `json:"direction,omitempty"`
	BlockedBy   string    `json:"blocked_by,omitempty"`
	MakerLagMS  int64     `json:"maker_lag_ms,omitempty"`
	TakerLagMS  int64     `json:"taker_lag_ms,omitempty"`
	LatencyMS   int64     `json:"latency_ms,omitempty"`
	NetBps      float64   `json:"net_bps,omitempty"`
	TotalQuote  string    `json:"total_quote,omitempty"`
}
```

- [ ] **Step 3: Extend the run session to create and expose `events.jsonl`**

```go
type Session struct {
	Dir          string
	RunLogPath   string
	RawPath      string
	EventsPath   string
	RunLogFile   *os.File
	RawFile      *os.File
	EventsFile   *os.File
}
```

- [ ] **Step 4: Record only session, round, pnl, and low-frequency health events**

```go
eventRecorder.Record(Event{TS: time.Now().UTC(), Kind: "session", Event: "startup", State: string(trader.State())})
eventRecorder.Record(Event{TS: time.Now().UTC(), Kind: "round", Event: "signal", RoundID: t.roundID, Direction: string(sig.Direction)})
eventRecorder.Record(Event{TS: time.Now().UTC(), Kind: "pnl", Event: "refresh", RoundID: p.rounds, NetBps: p.lastRoundNetBps})
eventRecorder.Record(Event{TS: time.Now().UTC(), Kind: "health", Event: "heartbeat", BlockedBy: status.Blocked, MakerLagMS: status.MakerLagMS, TakerLagMS: status.TakerLagMS})
```

- [ ] **Step 5: Run recorder and app tests to verify the new artifact stays event-based**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/runlog ./internal/app -run 'TestEventRecorder_WritesRoundEventJSONL|TestRun_LogOmitsPerFrameMarketDump' -count=1
```

Expected:

- `events.jsonl` is created
- events are flat and event-based
- no per-frame market snapshots are written to the structured event file

## Task 5: Final Verification and Documentation

**Files:**
- Modify: `README.md`
- Modify: `README_CN.md`
- Modify: `internal/app/run_test.go`

- [ ] **Step 1: Update documentation so the three log layers are explicit**

```md
- `run.log`: operator-facing heartbeat and lifecycle events
- `events.jsonl`: low-volume structured session / round / pnl / health events for replay
- `raw.jsonl`: raw orderbook evidence for deep debugging
```

- [ ] **Step 2: Run package-level verification for all touched packages**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/app ./internal/runlog ./internal/trading ./internal/marketdata
```

Expected:

- all touched packages pass

- [ ] **Step 3: Run a broader regression command before claiming completion**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

Expected:

- full repository test suite passes

- [ ] **Step 4: Manually inspect one temporary run artifact directory after a short local run**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/app -run TestRun_StartsTradingAccounts -count=1
```

Expected:

- generated `run.log` shows `STAT` and `EVT`, not per-frame `MKT`
- generated `events.jsonl` contains only session / round / pnl / health events
- `raw.jsonl` still contains the raw market stream

- [ ] **Step 5: Commit with a Lore-format message once verification is green**

```bash
git add docs/superpowers/specs/2026-04-06-runtime-logging-redesign-design.md docs/superpowers/plans/2026-04-06-runtime-logging-redesign.md internal/app/run.go internal/app/run_test.go internal/runlog/session.go internal/runlog/events.go internal/runlog/events_test.go internal/trading/status.go internal/trading/status_test.go internal/trading/trader.go internal/trading/open.go internal/trading/close.go internal/trading/pnl_tracker.go internal/trading/trader_test.go README.md README_CN.md
git commit -m "Make runtime logs answer operator questions first

The current runtime log optimizes for exhaustive market detail, which
buries the answers an operator needs during a live session. This change
shifts operator output toward heartbeat summaries and lifecycle events,
while keeping replay detail in dedicated structured artifacts.

Constraint: raw market evidence must remain available for postmortems
Rejected: keep per-frame MKT lines with lighter formatting | still too noisy for on-call scanning
Confidence: medium
Scope-risk: moderate
Reversibility: clean
Directive: Keep run.log focused on operator decisions; move replay detail into structured artifacts instead of re-expanding heartbeat lines
Tested: go test ./internal/app ./internal/runlog ./internal/trading ./internal/marketdata; go test ./...
Not-tested: live exchange behavior under sustained production quote bursts"
```

## Self-Review

- Spec coverage:
  - `run.log` operator-first contract is covered in Tasks 1 to 3
  - structured event stream is covered in Task 4
  - documentation and verification are covered in Task 5
- Placeholder scan:
  - no `TODO`, `TBD`, or deferred implementation markers remain
- Type consistency:
  - `safe`, `profit`, and `blocked` are introduced through one shared `operatorStatus` path
  - structured data uses one flat `Event` shape across session / round / pnl / health writes
