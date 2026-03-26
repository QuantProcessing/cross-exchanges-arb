# Decibel + Lighter Cross-Exchange Arbitrage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Upgrade the project to `github.com/QuantProcessing/exchanges v0.2.0`, replace hard-coded exchange wiring with generic perp exchange creation, and implement a single-position live-validation arbitrage loop that can complete `maker -> hedge -> close -> cooldown -> next round` safely.

**Architecture:** Keep strategy code on top of `exchanges.Exchange`, add a small generic exchange factory, define explicit execution state and validation-mode safeguards, and refactor `Trader` so signal handling and live execution are serialized through a state machine. Preserve the current spread model, but make signal production independent from order execution decisions.

**Tech Stack:** Go 1.26, `github.com/QuantProcessing/exchanges v0.2.0`, `github.com/shopspring/decimal`, `go.uber.org/zap`, Go `testing` package

---

## File Map

### Existing files to modify

- `go.mod`
- `go.sum`
- `main.go`
- `config.go`
- `spread_engine.go`
- `trader.go`
- `README.md`
- `README_CN.md`

### New files to create

- `exchange_factory.go`
- `exchange_factory_test.go`
- `execution_profile.go`
- `execution_state.go`
- `execution_state_test.go`
- `signal_filter.go`
- `signal_filter_test.go`
- `trader_test.go`

### Responsibility split

- `exchange_factory.go`: generic registry-based maker/taker construction from config
- `execution_profile.go`: generic execution defaults and validation-mode settings
- `execution_state.go`: explicit trader state enum, transition helpers, and risk-gate helpers
- `signal_filter.go`: decide whether a spread signal is executable under current profile and trader state
- `trader.go`: live order workflow, partial-fill hedge handling, close flow, manual-intervention blocking
- `spread_engine.go`: emit candidate signals only, no exchange-specific execution assumptions
- `config.go`: generic CLI/config parsing for exchange names, quote currencies, validation mode, round limits, maker timeout

## Task 1: Upgrade Dependency and Add Generic Exchange Factory

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `main.go`
- Modify: `config.go`
- Create: `exchange_factory.go`
- Test: `exchange_factory_test.go`

- [ ] **Step 1: Write the failing factory/config tests**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run 'Test(BuildExchangeConfigs_DefaultsToPerpValidationProfile|NewExchangePair_RejectsUnknownExchange)' -count=1`

Expected: FAIL because `BuildExchangeConfigs` and `NewExchangePair` do not exist yet.

- [ ] **Step 3: Upgrade to `exchanges v0.2.0`**

Run:

```bash
/usr/local/go/bin/go get github.com/QuantProcessing/exchanges@v0.2.0
/usr/local/go/bin/go mod tidy
```

Expected: `go.mod` and `go.sum` update cleanly.

- [ ] **Step 4: Implement generic exchange config and factory**

Add `exchange_factory.go` with:

```go
type ExchangeRuntimeConfig struct {
	Name          string
	MarketType    exchanges.MarketType
	QuoteCurrency string
	Options       map[string]string
}

func BuildExchangeConfigs(cfg *Config) (ExchangeRuntimeConfig, ExchangeRuntimeConfig) { /* ... */ }

func NewExchange(ctx context.Context, rc ExchangeRuntimeConfig) (exchanges.Exchange, error) { /* ... */ }

func NewExchangePair(ctx context.Context, cfg *Config) (exchanges.Exchange, exchanges.Exchange, error) { /* ... */ }
```

Implementation notes:

- Import exchange packages in `main.go` with blank imports so `init()` registration runs.
- Use `exchanges.LookupConstructor`.
- Keep strategy code unaware of exchange-specific constructors.
- Pull credentials from env into the options map at the config boundary only.

- [ ] **Step 5: Refactor startup wiring to use the factory**

Replace `createAdapter` in `main.go` with generic `NewExchangePair`.

- [ ] **Step 6: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run 'Test(BuildExchangeConfigs_DefaultsToPerpValidationProfile|NewExchangePair_RejectsUnknownExchange)' -count=1`

Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum main.go config.go exchange_factory.go exchange_factory_test.go
git commit -m "feat: add generic exchange factory"
```

## Task 2: Add Execution Profile and Validation-Mode Config

**Files:**
- Modify: `config.go`
- Create: `execution_profile.go`
- Test: `exchange_factory_test.go`

- [ ] **Step 1: Write the failing execution-profile tests**

```go
func TestDefaultExecutionProfile_UsesValidationDefaults(t *testing.T) {
	p := DefaultExecutionProfile()
	if !p.LiveValidation {
		t.Fatal("expected live validation enabled by default for validation profile")
	}
	if p.EntryMakerOrderType != exchanges.OrderTypePostOnly {
		t.Fatalf("entry type = %s, want post-only", p.EntryMakerOrderType)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run TestDefaultExecutionProfile_UsesValidationDefaults -count=1`

Expected: FAIL because `DefaultExecutionProfile` does not exist.

- [ ] **Step 3: Add config fields and execution profile model**

Add:

- `LiveValidate bool`
- `MakerTimeout time.Duration`
- `MaxRounds int`
- optional `MakerQuoteCurrency` / `TakerQuoteCurrency`

Add `execution_profile.go` with:

```go
type ExecutionProfile struct {
	LiveValidation     bool
	EntryMakerOrderType exchanges.OrderType
	HedgeUsesSlippage  bool
	MakerTimeout       time.Duration
	MaxRounds          int
}

func DefaultExecutionProfile() ExecutionProfile { /* ... */ }
```

- [ ] **Step 4: Wire config parsing and startup summary**

Update `Config.String()` so startup clearly shows validation mode, maker timeout, and max rounds.

- [ ] **Step 5: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run TestDefaultExecutionProfile_UsesValidationDefaults -count=1`

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add config.go execution_profile.go exchange_factory_test.go
git commit -m "feat: add validation execution profile"
```

## Task 3: Introduce Explicit Execution State and Risk Gates

**Files:**
- Create: `execution_state.go`
- Create: `execution_state_test.go`
- Modify: `trader.go`

- [ ] **Step 1: Write the failing state tests**

```go
func TestExecutionState_AllowsSignalOnlyWhenIdle(t *testing.T) {
	if CanAcceptSignal(StateWaitingFill) {
		t.Fatal("waiting_fill must not accept new signals")
	}
	if !CanAcceptSignal(StateIdle) {
		t.Fatal("idle must accept signals")
	}
}

func TestExecutionState_BlocksTradingInManualIntervention(t *testing.T) {
	if CanAcceptSignal(StateManualIntervention) {
		t.Fatal("manual intervention must block trading")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run 'TestExecutionState_' -count=1`

Expected: FAIL because the state types and helpers do not exist.

- [ ] **Step 3: Implement explicit execution states**

Create:

```go
type ExecutionState string

const (
	StateIdle               ExecutionState = "idle"
	StatePlacingMaker       ExecutionState = "placing_maker"
	StateWaitingFill        ExecutionState = "waiting_fill"
	StateHedging            ExecutionState = "hedging"
	StatePositionOpen       ExecutionState = "position_open"
	StateClosing            ExecutionState = "closing"
	StateCooldown           ExecutionState = "cooldown"
	StateManualIntervention ExecutionState = "manual_intervention"
)

func CanAcceptSignal(state ExecutionState) bool { /* ... */ }
func IsTerminalRoundBlocker(state ExecutionState) bool { /* ... */ }
```

- [ ] **Step 4: Add state to `Trader` without changing behavior yet**

Add `state ExecutionState` to `Trader` and initialize it to `StateIdle`.

- [ ] **Step 5: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run 'TestExecutionState_' -count=1`

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add execution_state.go execution_state_test.go trader.go
git commit -m "feat: add explicit execution states"
```

## Task 4: Separate Candidate Signals from Executable Signals

**Files:**
- Modify: `spread_engine.go`
- Create: `signal_filter.go`
- Create: `signal_filter_test.go`
- Modify: `main.go`

- [ ] **Step 1: Write the failing signal-filter tests**

```go
func TestSignalFilter_RejectsWhenTraderNotIdle(t *testing.T) {
	ok := IsExecutableSignal(StateWaitingFill, DefaultExecutionProfile(), &SpreadSignal{})
	if ok {
		t.Fatal("expected signal rejection while waiting_fill")
	}
}

func TestSignalFilter_AcceptsIdleSignal(t *testing.T) {
	ok := IsExecutableSignal(StateIdle, DefaultExecutionProfile(), &SpreadSignal{})
	if !ok {
		t.Fatal("expected idle signal to be executable")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run 'TestSignalFilter_' -count=1`

Expected: FAIL because the signal filter does not exist.

- [ ] **Step 3: Implement the filter and decouple engine callback**

Create `signal_filter.go`:

```go
func IsExecutableSignal(state ExecutionState, profile ExecutionProfile, sig *SpreadSignal) bool {
	/* ... */
}
```

Refactor `main.go` / `Trader` integration so:

- `SpreadEngine` emits candidate signals
- `Trader.HandleSignal` first passes through generic execution checks

- [ ] **Step 4: Keep `SpreadEngine` strategy-neutral**

Do not add exchange-name branching. Keep spread statistics and candidate signal emission generic.

- [ ] **Step 5: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run 'TestSignalFilter_' -count=1`

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add spread_engine.go signal_filter.go signal_filter_test.go main.go trader.go
git commit -m "refactor: decouple spread signals from execution"
```

## Task 5: Refactor Open Flow into an Explicit State Machine

**Files:**
- Modify: `trader.go`
- Create: `trader_test.go`
- Modify: `execution_state.go`

- [ ] **Step 1: Write the failing open-flow tests**

```go
func TestTrader_MakerTimeoutReturnsToIdle(t *testing.T) {
	tr := newTestTrader()
	tr.state = StateWaitingFill

	tr.handleMakerTimeoutForTest()

	if tr.state != StateIdle {
		t.Fatalf("state = %s, want idle", tr.state)
	}
}

func TestTrader_PartialFillTriggersImmediateHedge(t *testing.T) {
	tr := newTestTrader()
	err := tr.handleMakerFillForTest(decimal.RequireFromString("0.001"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.state != StatePositionOpen {
		t.Fatalf("state = %s, want position_open", tr.state)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run 'TestTrader_(MakerTimeoutReturnsToIdle|PartialFillTriggersImmediateHedge)' -count=1`

Expected: FAIL because the state-machine helpers do not exist.

- [ ] **Step 3: Refactor `HandleSignal` and open path**

Implementation targets:

- `HandleSignal` only transitions `idle -> placing_maker`
- `openMakerTaker` becomes a state-driven open workflow
- set `StateWaitingFill` after successful maker placement
- on full or partial fill, hedge actual filled quantity immediately
- on hedge success, transition to `StatePositionOpen`

- [ ] **Step 4: Handle maker timeout and cancel safely**

Ensure timeout path:

- cancels maker
- waits for terminal status
- hedges if a fill happened during cancel
- otherwise returns to `StateIdle`

- [ ] **Step 5: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run 'TestTrader_(MakerTimeoutReturnsToIdle|PartialFillTriggersImmediateHedge)' -count=1`

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add trader.go trader_test.go execution_state.go
git commit -m "feat: implement state-driven open workflow"
```

## Task 6: Add Manual-Intervention and Close-Failure Blocking

**Files:**
- Modify: `trader.go`
- Modify: `trader_test.go`
- Modify: `execution_state.go`

- [ ] **Step 1: Write the failing failure-path tests**

```go
func TestTrader_HedgeFailureMovesToManualIntervention(t *testing.T) {
	tr := newTestTrader()
	tr.forceHedgeError = errors.New("boom")

	_ = tr.handleMakerFillForTest(decimal.RequireFromString("0.001"))

	if tr.state != StateManualIntervention {
		t.Fatalf("state = %s, want manual_intervention", tr.state)
	}
}

func TestTrader_CloseFailureBlocksNextRound(t *testing.T) {
	tr := newTestTraderWithPosition()
	tr.forceCloseError = errors.New("close failed")

	tr.closePosition("test")

	if tr.state == StateIdle {
		t.Fatal("close failure must not return to idle")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run 'TestTrader_(HedgeFailureMovesToManualIntervention|CloseFailureBlocksNextRound)' -count=1`

Expected: FAIL because failure-path state handling is incomplete.

- [ ] **Step 3: Implement hedge-failure blocking**

When maker fill succeeded but hedge fails:

- set `StateManualIntervention`
- preserve enough residual position context for alerting
- reject all new signals

- [ ] **Step 4: Implement close-failure blocking**

If either close leg fails:

- do not clear position blindly
- keep state out of `StateIdle`
- alert and block the next round

- [ ] **Step 5: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run 'TestTrader_(HedgeFailureMovesToManualIntervention|CloseFailureBlocksNextRound)' -count=1`

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add trader.go trader_test.go execution_state.go
git commit -m "feat: block trading after unresolved execution failures"
```

## Task 7: Implement Cooldown, Round Limits, and Live-Validation Loop Controls

**Files:**
- Modify: `trader.go`
- Modify: `config.go`
- Modify: `trader_test.go`

- [ ] **Step 1: Write the failing validation-loop tests**

```go
func TestTrader_SuccessfulCloseTransitionsToCooldown(t *testing.T) {
	tr := newTestTraderWithPosition()

	tr.closePosition("done")

	if tr.state != StateCooldown {
		t.Fatalf("state = %s, want cooldown", tr.state)
	}
}

func TestTrader_MaxRoundsStopsNewTrading(t *testing.T) {
	tr := newTestTrader()
	tr.completedRounds = 1
	tr.profile.MaxRounds = 1

	if tr.canStartNextRound() {
		t.Fatal("expected trading to stop after max rounds")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/usr/local/go/bin/go test ./... -run 'TestTrader_(SuccessfulCloseTransitionsToCooldown|MaxRoundsStopsNewTrading)' -count=1`

Expected: FAIL because cooldown and round-limit controls are incomplete.

- [ ] **Step 3: Implement cooldown and round counting**

Requirements:

- successful close sets `StateCooldown`
- cooldown expiry returns state to `StateIdle`
- completed successful round count increments once per fully resolved open+close loop
- `MaxRounds` blocks further rounds in validation mode

- [ ] **Step 4: Run targeted tests**

Run: `/usr/local/go/bin/go test ./... -run 'TestTrader_(SuccessfulCloseTransitionsToCooldown|MaxRoundsStopsNewTrading)' -count=1`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add trader.go trader_test.go config.go
git commit -m "feat: add live validation round controls"
```

## Task 8: Update Docs and Run Full Verification

**Files:**
- Modify: `README.md`
- Modify: `README_CN.md`
- Possibly modify: `main.go`

- [ ] **Step 1: Write the failing docs expectation checklist**

Checklist to satisfy:

- README documents `exchanges v0.2.0` generic exchange support
- README documents live-validation mode and its safety limits
- README documents default first validation pair: `DECIBEL` maker, `LIGHTER` taker
- README warns that unresolved failures block the next round

- [ ] **Step 2: Update docs**

Add examples like:

```bash
/usr/local/go/bin/go run . \
  --maker DECIBEL \
  --taker LIGHTER \
  --symbol BTC \
  --qty 0.001 \
  --live-validate \
  --maker-timeout 15s \
  --max-rounds 1
```

- [ ] **Step 3: Run full test suite**

Run: `GOCACHE=/tmp/gocache-cross-arb /usr/local/go/bin/go test ./...`

Expected: PASS

- [ ] **Step 4: Run build verification**

Run: `/usr/local/go/bin/go build ./...`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add README.md README_CN.md main.go
git commit -m "docs: document live validation workflow"
```

## Final Verification Checklist

- [ ] `go.mod` points to `github.com/QuantProcessing/exchanges v0.2.0`
- [ ] `main.go` no longer hard-codes specific adapter constructors
- [ ] generic factory creates registered perp exchanges
- [ ] trader has explicit serialized execution states
- [ ] partial fill hedges actual filled quantity immediately
- [ ] hedge failure moves the system into a blocked state
- [ ] close failure blocks the next round
- [ ] cooldown and max-round validation controls work
- [ ] README and README_CN explain live-validation behavior
- [ ] `GOCACHE=/tmp/gocache-cross-arb /usr/local/go/bin/go test ./...` passes
- [ ] `/usr/local/go/bin/go build ./...` passes
