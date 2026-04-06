# TradingAccount migration (exchanges v0.2.10) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Upgrade to exchanges v0.2.10 and migrate all order/position handling to `account.TradingAccount` with WS order placement for open/hedge/close flows.

**Architecture:** The runtime still builds maker/taker `exchanges.Exchange` adapters for market data, fees, and symbol details, but trading uses `account.TradingAccount` as the canonical order/position state. Trader uses `PlaceWS` + `OrderFlow` for every order and validates positions from TradingAccount snapshots.

**Tech Stack:** Go 1.26, github.com/QuantProcessing/exchanges v0.2.10, existing internal trading modules.

---

## File structure map
- **Modify** `go.mod`, `go.sum` — bump exchanges to v0.2.10.
- **Modify** `internal/app/run.go` — construct and start maker/taker TradingAccount, pass to Trader.
- **Modify** `internal/trading/trader.go` — Trader holds TradingAccount references and subscriptions instead of WatchOrders/WatchFills.
- **Modify** `internal/trading/open.go` — use `PlaceWS` + orderFlow for maker/hedge; remove direct order channel handling; update position verification to use TradingAccount snapshots.
- **Modify** `internal/trading/close.go` — use `PlaceWS` + orderFlow for close legs; remove REST/stream confirm path.
- **Modify** `internal/trading/trader_test.go` — update tests to use TradingAccount flow helpers and new state transitions.
- **Modify** `internal/app/run_test.go`, `internal/marketdata/recorder_test.go` — update test doubles for new interface methods (`CancelOrderWS` already required by v0.2.10).

**Known baseline failures (after `go get`):** test doubles do not implement `CancelOrderWS` (marketdata/app/trading tests). Address during implementation.

---

### Task 1: Upgrade exchanges dependency

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Write failing test**

No new test. Dependency update only.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./...`
Expected: FAIL (baseline: missing `CancelOrderWS` on test doubles).

- [ ] **Step 3: Update dependency**

In `go.mod`, ensure:
```go
require (
    github.com/QuantProcessing/exchanges v0.2.10
    // ... other deps
)
```

- [ ] **Step 4: Run test to verify it fails (same baseline)**

Run: `go test ./...`
Expected: FAIL with same baseline errors (until test doubles updated).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: bump exchanges to v0.2.10"
```

---

### Task 2: Introduce TradingAccount in app wiring

**Files:**
- Modify: `internal/app/run.go:29-90`

- [ ] **Step 1: Write failing test**

Update `internal/app/run_test.go` to assert TradingAccount start is invoked. Add a new test double implementing `account.TradingAccount` semantics or wrap existing exchange stub with a spy.

Example (add near existing run tests):
```go
type accountSpy struct {
    started bool
}

func (a *accountSpy) Start(ctx context.Context) error {
    a.started = true
    return nil
}
func (a *accountSpy) Close() {}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app -run TestRunStartsTradingAccount -v`
Expected: FAIL (Trader does not start accounts yet).

- [ ] **Step 3: Implement TradingAccount wiring**

In `internal/app/run.go`, after creating adapters:
```go
makerAccount := account.NewTradingAccount(maker, logger)
takerAccount := account.NewTradingAccount(taker, logger)
if err := makerAccount.Start(ctx); err != nil {
    return fmt.Errorf("start maker account: %w", err)
}
if err := takerAccount.Start(ctx); err != nil {
    return fmt.Errorf("start taker account: %w", err)
}
defer makerAccount.Close()
defer takerAccount.Close()
```

Then update Trader construction:
```go
trader := trading.NewTrader(maker, taker, makerAccount, takerAccount, engine, cfg, logger)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/app -run TestRunStartsTradingAccount -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/app/run.go internal/app/run_test.go
git commit -m "feat: start trading accounts in app run"
```

---

### Task 3: Trader owns TradingAccount + subscriptions

**Files:**
- Modify: `internal/trading/trader.go:40-170`

- [ ] **Step 1: Write failing test**

Add a test in `internal/trading/trader_test.go` that ensures Trader starts account subscriptions and uses them to route order updates.

Example test scaffold:
```go
func TestTrader_Start_SubscribeOrders(t *testing.T) {
    makerEx := newTestExchange()
    takerEx := newTestExchange()
    makerAcc := account.NewTradingAccount(makerEx, exchanges.NopLogger)
    takerAcc := account.NewTradingAccount(takerEx, exchanges.NopLogger)

    tr := NewTrader(makerEx, takerEx, makerAcc, takerAcc, nil, testConfig(), zap.NewNop().Sugar())
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    require.NoError(t, makerAcc.Start(ctx))
    require.NoError(t, takerAcc.Start(ctx))
    require.NoError(t, tr.Start(ctx))

    // publish a fake order update into makerAcc and assert trader observes it
    // (use makerAcc.SubscribeOrders or a helper hook if available)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trading -run TestTrader_Start_SubscribeOrders -v`
Expected: FAIL (Trader does not subscribe yet).

- [ ] **Step 3: Implement minimal Trader struct changes**

Update `internal/trading/trader.go` struct and constructor:
```go
type Trader struct {
    maker exchanges.Exchange
    taker exchanges.Exchange
    makerAccount *account.TradingAccount
    takerAccount *account.TradingAccount
    // ...
    makerOrdersSub *account.Subscription[exchanges.Order]
    takerOrdersSub *account.Subscription[exchanges.Order]
}

func NewTrader(maker, taker exchanges.Exchange, makerAcc, takerAcc *account.TradingAccount, engine MarketDataEngine, cfg *appconfig.Config, logger *zap.SugaredLogger) *Trader {
    // assign accounts
}
```

In `Start`, remove `WatchOrders` wiring and instead:
```go
if t.makerAccount != nil {
    t.makerOrdersSub = t.makerAccount.SubscribeOrders()
}
if t.takerAccount != nil {
    t.takerOrdersSub = t.takerAccount.SubscribeOrders()
}
```

Update shutdown/cleanup to unsubscribe (if needed):
```go
if t.makerOrdersSub != nil { t.makerOrdersSub.Unsubscribe() }
if t.takerOrdersSub != nil { t.takerOrdersSub.Unsubscribe() }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/trading -run TestTrader_Start_SubscribeOrders -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/trading/trader.go internal/trading/trader_test.go
git commit -m "refactor: subscribe to trading account order streams"
```

---

### Task 4: Migrate open flow to PlaceWS + OrderFlow

**Files:**
- Modify: `internal/trading/open.go`
- Modify: `internal/trading/trader.go`

- [ ] **Step 1: Write failing test**

Add test to validate maker partial fill triggers taker hedge via orderFlow:
```go
func TestOpenFlow_PartialFill_HedgesViaOrderFlow(t *testing.T) {
    // setup trader with maker/taker TradingAccount
    // create maker flow, emit partial fill, assert hedge PlaceWS called with delta
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trading -run TestOpenFlow_PartialFill_HedgesViaOrderFlow -v`
Expected: FAIL.

- [ ] **Step 3: Replace maker PlaceOrder with PlaceWS**

In `openMakerTaker`, replace:
```go
makerOrder, err := t.maker.PlaceOrder(ctx, &exchanges.OrderParams{...})
```
with:
```go
cid := exchanges.GenerateID()
makerFlow, err := t.makerAccount.PlaceWS(ctx, &exchanges.OrderParams{
    Symbol: t.config.Symbol,
    Side: makerSide,
    Type: exchanges.OrderTypePostOnly,
    Price: makerPrice,
    Quantity: qty,
    TimeInForce: exchanges.TimeInForcePO,
    ClientID: cid,
})
```

Store `makerFlow` in `openFlowState`:
```go
makerFlow *account.OrderFlow
```

- [ ] **Step 4: Handle maker flow updates**

Replace maker order channel handling with flow subscription:
```go
go func() {
    for update := range makerFlow.Updates() {
        // translate update into handleMakerOrderUpdate using order data
    }
}()
```

Use `makerFlow.Done()` to detect terminal status and finalize/reset accordingly.

- [ ] **Step 5: Replace hedge placement with PlaceWS**

In `hedgeMakerDelta`, replace `PlaceMarketOrderWithSlippage` with:
```go
cid := exchanges.GenerateID()
hedgeFlow, err := t.takerAccount.PlaceWS(ctx, &exchanges.OrderParams{
    Symbol: t.config.Symbol,
    Side: flow.takerSide,
    Type: exchanges.OrderTypeMarket,
    Quantity: delta,
    Slippage: slippage,
    ReduceOnly: false,
    ClientID: cid,
})
```

Wait for hedgeFlow terminal status instead of `waitTakerFill`.

- [ ] **Step 6: Update position verification to TradingAccount**

Replace `verifyTakerPosition` with:
```go
pos, ok := t.takerAccount.Position(t.config.Symbol)
if !ok { return false, fmt.Errorf("no taker position found") }
if pos.Quantity.Abs().GreaterThanOrEqual(totalExpected) { return true, nil }
return false, fmt.Errorf("taker position %s < expected %s", pos.Quantity.Abs(), totalExpected)
```

- [ ] **Step 7: Run test to verify it passes**

Run: `go test ./internal/trading -run TestOpenFlow_PartialFill_HedgesViaOrderFlow -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/trading/open.go internal/trading/trader.go internal/trading/trader_test.go
git commit -m "feat: migrate open flow to TradingAccount PlaceWS"
```

---

### Task 5: Migrate close flow to PlaceWS + OrderFlow

**Files:**
- Modify: `internal/trading/close.go`

- [ ] **Step 1: Write failing test**

Add test validating both close legs wait for orderFlow terminal states.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trading -run TestCloseFlow_UsesOrderFlow -v`
Expected: FAIL.

- [ ] **Step 3: Replace close leg PlaceOrder with PlaceWS**

Replace each `PlaceOrder` call with `PlaceWS` and keep the returned `OrderFlow`:
```go
flow, err := account.PlaceWS(ctx, &exchanges.OrderParams{... ClientID: exchanges.GenerateID() ...})
```

Wait on `flow.Done()` and read final order via `flow.Latest()` (or last update received).

- [ ] **Step 4: Remove REST snapshot confirm**

Delete `confirmOrderFilled` and `fetchOrderSnapshot` usage for close; use flow terminal status instead.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/trading -run TestCloseFlow_UsesOrderFlow -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/trading/close.go internal/trading/trader_test.go
git commit -m "feat: migrate close flow to TradingAccount PlaceWS"
```

---

### Task 6: Update test doubles for exchanges v0.2.10

**Files:**
- Modify: `internal/trading/trader_test.go`
- Modify: `internal/app/run_test.go`
- Modify: `internal/marketdata/recorder_test.go`

- [ ] **Step 1: Write failing test**

Existing tests fail due to missing `CancelOrderWS` and new TradingAccount behaviors. No new test required.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./...`
Expected: FAIL with missing `CancelOrderWS` (baseline).

- [ ] **Step 3: Implement interface fixes**

Add `CancelOrderWS` to test exchanges, matching signature:
```go
func (t *testExchange) CancelOrderWS(ctx context.Context, orderID, symbol string) error {
    return t.CancelOrder(ctx, orderID, symbol)
}
```

Update test exchange stubs to support `PlaceOrderWS` if needed, and to emit order updates to TradingAccount flows.

- [ ] **Step 4: Run test to verify it passes (or advances)**

Run: `go test ./...`
Expected: Remaining failures now point to TradingAccount migration changes rather than interface mismatch.

- [ ] **Step 5: Commit**

```bash
git add internal/trading/trader_test.go internal/app/run_test.go internal/marketdata/recorder_test.go
git commit -m "test: update exchange stubs for v0.2.10"
```

---

## Self-review checklist (plan vs spec)
- **Spec coverage:** Tasks 1–6 cover dependency bump, TradingAccount wiring, WS order flows, and position verification.
- **Placeholder scan:** No TODO/TBD placeholders remain.
- **Type consistency:** `account.TradingAccount`, `PlaceWS`, `OrderFlow`, and `Subscription` names are consistent with v0.2.10.

---

Plan complete and saved to `docs/superpowers/plans/2026-04-05-exchanges-tradingaccount-upgrade.md`. Two execution options:

1. **Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration
2. **Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
