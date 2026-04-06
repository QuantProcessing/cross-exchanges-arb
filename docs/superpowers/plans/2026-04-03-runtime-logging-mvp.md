# Runtime Logging MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-run artifact directory with `run.log` and `metrics.jsonl`, upgrade to `exchanges v0.2.8`, and log order/fill latency details using both local and exchange timestamps.

**Architecture:** Keep the MVP intentionally small. `run.log` becomes the primary operator-facing log, while `metrics.jsonl` becomes a replay-oriented stream of orderbook updates plus rolling spread statistics. Trading code emits richer runtime logs, but only market/engine records are persisted to the replay file.

**Tech Stack:** Go 1.26, `github.com/QuantProcessing/exchanges`, Zap logging, JSONL writers, existing `internal/app`, `internal/marketdata`, `internal/spread`, and `internal/trading` packages

---

## File Map

### Existing files to modify

- `go.mod`
- `go.sum`
- `main.go`
- `internal/app/run.go`
- `internal/app/run_test.go`
- `internal/marketdata/recorder.go`
- `internal/marketdata/schema.go`
- `internal/spread/engine.go`
- `internal/spread/types.go`
- `internal/trading/open.go`
- `internal/trading/close.go`
- `internal/trading/trader.go`
- `internal/trading/trader_test.go`
- `README.md`
- `README_CN.md`

### New files to create

- `internal/runlog/session.go`
- `internal/runlog/session_test.go`

## Task 1: Add Run Artifact Session

**Files:**
- Create: `internal/runlog/session.go`
- Test: `internal/runlog/session_test.go`
- Modify: `internal/app/run.go`
- Modify: `main.go`

- [ ] **Step 1: Write failing tests for run artifact paths and file creation**
- [ ] **Step 2: Run the targeted tests and watch them fail**
- [ ] **Step 3: Implement `runlog.Session` with run directory, `run.log`, and `metrics.jsonl` handles**
- [ ] **Step 4: Wire the session into startup so every run gets an isolated directory**
- [ ] **Step 5: Re-run the targeted tests and verify they pass**

## Task 2: Simplify Metrics Recording

**Files:**
- Modify: `internal/marketdata/schema.go`
- Modify: `internal/marketdata/recorder.go`
- Modify: `internal/spread/types.go`
- Modify: `internal/spread/engine.go`
- Modify: `internal/app/run.go`
- Test: `internal/app/run_test.go`

- [ ] **Step 1: Write failing tests for `metrics.jsonl` records containing market books plus spread/mean/std/z fields**
- [ ] **Step 2: Run the targeted tests and watch them fail**
- [ ] **Step 3: Extend the metrics record schema to include quote lag and rolling stats**
- [ ] **Step 4: Update the recorder path so it writes only replay-oriented orderbook/stat records**
- [ ] **Step 5: Re-run the targeted tests and verify they pass**

## Task 3: Upgrade `exchanges` and Add Fill-Aware Runtime Logging

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `internal/trading/trader.go`
- Modify: `internal/trading/open.go`
- Modify: `internal/trading/close.go`
- Modify: `internal/trading/trader_test.go`

- [ ] **Step 1: Upgrade `github.com/QuantProcessing/exchanges` to `v0.2.8` and inspect `WatchFills` / fill types**
- [ ] **Step 2: Write failing trader tests for logging order/fill details with latency fields**
- [ ] **Step 3: Subscribe to both `WatchOrders` and `WatchFills`**
- [ ] **Step 4: Track local submit/ack/fill times and exchange timestamps, then log:
  - submit to exchange
  - exchange to local ack
  - exchange to local fill
  - submit to first fill (exchange and local views)
  - close-leg latency details**
- [ ] **Step 5: Re-run the targeted trader tests and verify they pass**

## Task 4: Final Wiring and Documentation

**Files:**
- Modify: `README.md`
- Modify: `README_CN.md`
- Modify: `internal/app/run_test.go`

- [ ] **Step 1: Update documentation to describe the per-run `logs/.../run.log` and `metrics.jsonl` outputs**
- [ ] **Step 2: Run package-level verification for touched packages**
- [ ] **Step 3: Run a broader regression command covering the full repo**
- [ ] **Step 4: Review the emitted files and residual risks before reporting completion**
