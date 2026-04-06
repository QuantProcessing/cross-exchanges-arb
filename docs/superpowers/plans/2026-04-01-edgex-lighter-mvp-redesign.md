# EDGEX + LIGHTER Arbitrage MVP Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace raw-BBO z-score trading with a safer MVP that trades only on sanitized, executable spread and uses moving-average / rolling-standard-deviation as dynamic thresholds rather than the sole signal.

**Architecture:** Add a quote-sanitation stage before signal generation, introduce executable-spread modeling and direction gating, then refactor entry/exit approval so dynamic thresholds are applied to sanitized executable spread. Keep the current single-position live-validation workflow, but add execution-quality monitoring and automatic pause conditions.

**Tech Stack:** Go 1.26, existing `exchanges` integration, Go `testing`, existing `spread` and `trading` packages, structured Zap logging

---

## File Map

### Existing files to modify

- `internal/spread/types.go`
- `internal/spread/engine.go`
- `internal/trading/open.go`
- `internal/trading/close.go`
- `internal/trading/recap.go`
- `internal/config/config.go`
- `README.md`
- `README_CN.md`

### New files to create

- `internal/spread/sanitize.go`
- `internal/spread/sanitize_test.go`
- `internal/spread/executable_edge.go`
- `internal/spread/executable_edge_test.go`
- `internal/trading/execution_guard.go`
- `internal/trading/execution_guard_test.go`

## Task 1: Add Quote Sanitation and Invalid-Direction Blocking

**Files:**
- Create: `internal/spread/sanitize.go`
- Test: `internal/spread/sanitize_test.go`
- Modify: `internal/spread/types.go`
- Modify: `internal/spread/engine.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: Write the failing sanitation tests**

Add tests covering:

- crossed book: `ask < bid` must be rejected
- stale quote: old timestamp must be rejected
- too-small size proxy or missing valid top-of-book must be rejected
- direction policy: `LONG_TAKER_SHORT_MAKER` disabled by default for EDGEX/LIGHTER

- [ ] **Step 2: Run the tests and verify they fail**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/spread -run 'Test(RejectCrossedQuote|RejectStaleQuote|DirectionPolicyBlocksLongTakerShortMaker)' -count=1
```

Expected: FAIL because sanitation and direction policy do not exist yet.

- [ ] **Step 3: Implement sanitized quote model**

Add:

- `SanitizedQuote`
- `SanitizedSnapshot`
- rejection reasons / quality flags

Minimum checks:

- `bid > 0`
- `ask > 0`
- `ask >= bid`
- optional freshness threshold

- [ ] **Step 4: Block invalid direction by default**

For the EDGEX/LIGHTER profile:

- keep both directions available
- filter crossed / stale / too-small quotes before either direction can be considered executable

This preserves directional symmetry while removing the abnormal-quote path that created fake `LONG_TAKER_SHORT_MAKER` opportunities.

- [ ] **Step 5: Re-run the targeted tests**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/spread -run 'Test(RejectCrossedQuote|RejectStaleQuote|DirectionPolicyBlocksLongTakerShortMaker)' -count=1
```

Expected: PASS

## Task 2: Replace Raw Spread With Executable Spread Approval

**Files:**
- Create: `internal/spread/executable_edge.go`
- Test: `internal/spread/executable_edge_test.go`
- Modify: `internal/spread/engine.go`
- Modify: `internal/spread/types.go`

- [ ] **Step 1: Write failing executable-edge tests**

Cover:

- long maker / short taker edge uses sanitized buy and sell prices
- invalid quote produces no executable edge
- raw BA can be large while executable BA is near zero once `ask=max(ask,bid)` or invalid quotes are rejected

- [ ] **Step 2: Run the tests and verify they fail**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/spread -run 'Test(ExecutableEdge|InvalidQuoteRemovesEdge|RawBAFallsAwayAfterSanitization)' -count=1
```

Expected: FAIL because executable-edge logic does not exist.

- [ ] **Step 3: Implement executable edge calculation**

Add helpers that compute:

- executable open edge
- sanitized moving-average input value
- fee-adjusted net edge

The first version may use top-level quantity proxy if full depth is unavailable, but it must not use invalid quotes.

- [ ] **Step 4: Refactor signal generation**

Refactor `engine.go` so:

- rolling mean/std consume sanitized executable spread
- raw invalid snapshots do not enter the stats ring
- signal candidates carry both:
  - executable spread
  - dynamic threshold context

- [ ] **Step 5: Re-run the targeted tests**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/spread -run 'Test(ExecutableEdge|InvalidQuoteRemovesEdge|RawBAFallsAwayAfterSanitization)' -count=1
```

Expected: PASS

## Task 3: Make Entry Dynamic But Hard-Gated

**Files:**
- Modify: `internal/spread/engine.go`
- Modify: `internal/config/config.go`
- Create: `internal/trading/execution_guard.go`
- Test: `internal/trading/execution_guard_test.go`

- [ ] **Step 1: Write failing entry-approval tests**

Cover:

- signal must satisfy both dynamic threshold and hard minimum net open bps
- a persistent spread that is large in absolute terms but below sigma gate should still be tradable if policy explicitly allows absolute-edge override
- a high sigma signal with weak executable net edge must be rejected

- [ ] **Step 2: Run the tests and verify they fail**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'Test(EntryRequiresDynamicAndHardEdge|WeakExecutableEdgeRejected|AbsoluteEdgeOverride)' -count=1
```

Expected: FAIL because the new guard does not exist.

- [ ] **Step 3: Add config knobs**

Add configuration for:

- `z-open`
- `z-close`
- `min-net-open-bps`
- `impact-buffer-bps`
- `latency-buffer-bps`

- [ ] **Step 4: Implement entry guard**

The guard must approve entry only if:

- quote quality is valid
- direction is allowed
- executable edge exceeds hard costs and buffers
- dynamic threshold passes

- [ ] **Step 5: Re-run the targeted tests**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'Test(EntryRequiresDynamicAndHardEdge|WeakExecutableEdgeRejected|AbsoluteEdgeOverride)' -count=1
```

Expected: PASS

## Task 4: Replace Quote-Z Close Logic With Executable Exit Logic

**Files:**
- Modify: `internal/trading/close.go`
- Modify: `internal/trading/open.go`
- Modify: `internal/spread/types.go`
- Test: `internal/trading/execution_guard_test.go`

- [ ] **Step 1: Write failing close-condition tests**

Cover:

- profitable executable exit triggers close
- quote-Z reversion alone does not trigger close if executable edge is still bad
- stop-loss triggers on executable deterioration

- [ ] **Step 2: Run the tests and verify they fail**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'Test(ExecutableProfitClose|QuoteZAloneDoesNotClose|ExecutableStopLoss)' -count=1
```

Expected: FAIL because close logic still keys off quote Z.

- [ ] **Step 3: Implement executable exit decision**

Refactor close checks to use:

- executable exit edge
- realized-risk guardrails
- max hold

Z-score may remain an input, but not the sole close trigger.

- [ ] **Step 4: Re-run the targeted tests**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'Test(ExecutableProfitClose|QuoteZAloneDoesNotClose|ExecutableStopLoss)' -count=1
```

Expected: PASS

## Task 5: Add Expected-vs-Actual Execution Quality Monitoring

**Files:**
- Modify: `internal/trading/open.go`
- Modify: `internal/trading/close.go`
- Modify: `internal/trading/recap.go`
- Test: `internal/trading/trader_test.go`

- [ ] **Step 1: Write failing recap / guard tests**

Cover:

- every round stores quoted executable edge and actual fill edge
- large quote-to-fill deviation is logged
- repeated large deviations trip a pause / manual-intervention guard

- [ ] **Step 2: Run the tests and verify they fail**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'Test(RecapCapturesExpectedVsActual|ExecutionDriftTripsGuard)' -count=1
```

Expected: FAIL because the drift monitor does not exist.

- [ ] **Step 3: Implement execution-quality monitoring**

Track:

- approved executable spread
- maker fill spread contribution
- hedge quote
- hedge fill
- quote-to-fill deviation

Add a kill-switch policy for repeated abnormal drift.

- [ ] **Step 4: Re-run the targeted tests**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/trading -run 'Test(RecapCapturesExpectedVsActual|ExecutionDriftTripsGuard)' -count=1
```

Expected: PASS

## Task 6: Update Docs and Validate the Full MVP

**Files:**
- Modify: `README.md`
- Modify: `README_CN.md`
- Modify: `docs/superpowers/specs/2026-04-01-edgex-lighter-mvp-redesign-design.md`

- [ ] **Step 1: Document the new signal model**

Document:

- sanitized quote requirement
- executable spread
- dynamic threshold
- expected-vs-actual execution monitoring
- default disabled `LONG_TAKER_SHORT_MAKER`

- [ ] **Step 2: Run focused package tests**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./internal/spread ./internal/trading ./internal/config ./internal/app
```

Expected: PASS

- [ ] **Step 3: Run full regression**

Run:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

Expected: PASS

- [ ] **Step 4: Run diagnostics on modified files**

Run diagnostics for:

- `internal/spread/engine.go`
- `internal/spread/sanitize.go`
- `internal/spread/executable_edge.go`
- `internal/trading/open.go`
- `internal/trading/close.go`
- `internal/trading/execution_guard.go`

Expected: zero errors

- [ ] **Step 5: Commit**

Use a Lore-style commit message describing why the redesign shifts from raw quote Z-score to sanitized executable spread.
