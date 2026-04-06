# Runtime Logging Redesign Design

**Date:** 2026-04-06

## Goal

Refocus runtime logging around the operator on call. `run.log` should answer within 5 seconds whether the bot is safe, whether it is making money, and where it is blocked, while structured data remains useful for replay and postmortem analysis without turning into an overdesigned telemetry system.

## Current state summary

- `run.log` mixes operator-facing status with replay-grade detail.
- `MKT ...` lines are emitted on every unified orderbook update and dominate the file.
- Trading lifecycle logs (`signal`, `maker`, `fill`, `hedge`, `close`, `pnl`) are present, but they are easy to miss inside the market-state stream.
- `raw.jsonl` already preserves high-frequency orderbook evidence.
- Open/close recap logic already preserves rich per-round execution context.

## Design principles

- Separate operator logs from replay data.
- Prefer low-frequency summaries plus state transitions over per-tick dumps.
- Keep structured logging event-based, not tick-based.
- Reuse existing artifacts (`run.log`, `raw.jsonl`, round recap) instead of adding a new telemetry subsystem.
- Optimize for deletion and simplification before adding new log types.

## Target logging layers

### 1. `run.log` is the operator stream

`run.log` becomes a compact text stream with only:

- low-frequency `STAT` heartbeat lines
- important lifecycle `EVT` lines
- warnings and errors that require attention

It should no longer print the full market snapshot on every frame.

### 2. Structured events are the replay stream

Structured data should record significant state changes and evidence needed for postmortems:

- session lifecycle
- round lifecycle
- realized PnL updates
- low-frequency health snapshots

It should not persist every orderbook-derived metric on every tick.

### 3. `raw.jsonl` remains the evidence stream

`raw.jsonl` continues to hold raw market snapshots for offline analysis and deep debugging. It remains the place to inspect per-side book updates and quote timestamps.

## `run.log` format

### `STAT` heartbeat

Heartbeat lines should be emitted on a low fixed cadence such as every 1 to 5 seconds.

Example:

```text
STAT safe=SAFE profit=round:open total:+12.4bps state=WAIT_HEDGE blocked=hedge_wait_fill maker_lag=12ms taker_lag=84ms rounds=3
STAT safe=DANGER profit=round:-6.1bps total:+5.2bps state=MANUAL blocked=close_wait_confirm maker_lag=15ms taker_lag=41ms rounds=4
STAT safe=SAFE profit=total:+22.8bps state=IDLE blocked=none maker_lag=6ms taker_lag=18ms rounds=5
```

Required heartbeat fields:

- `safe`
- `profit`
- `state`
- `blocked`
- `maker_lag`
- `taker_lag`
- `rounds`

Optional fields may be added only if they materially improve operator decisions.

### `EVT` lifecycle lines

State transitions should emit a single operator-readable event line.

Example:

```text
EVT warmup_done ticks=200 elapsed=14s
EVT signal round=7 dir=LONG_MAKER_SHORT_TAKER edge=+11.2bps qty=0.02
EVT maker_placed round=7 px=66960.1 qty=0.02
EVT maker_fill round=7 filled=0.02 wait=420ms
EVT hedge_done round=7 wait=180ms
EVT close_done round=7 reason=target_reached net=+9.6bps total=+31.4bps
EVT risk_pause reason=taker_stale
EVT risk_resume reason=quotes_fresh
```

`EVT` lines should cover:

- startup and shutdown
- warmup progress milestones and completion
- signal accepted
- maker placed
- maker fill or cancel outcome
- hedge done or hedge failure
- close start and close done
- round failed / manual intervention
- risk pause / risk resume
- periodic PnL refresh

## Core operator verdicts

### `safe`

`safe` is not a pure healthcheck. It is an operator verdict.

- `SAFE`: no immediate action needed
- `WARN`: running, but degraded or requires closer watching
- `DANGER`: human attention is required now

Recommended mapping:

- `SAFE`
  - no unhedged exposure
  - no unresolved close failure
  - quote freshness within thresholds
  - bot not in manual intervention state
- `WARN`
  - maker or taker quote lag over threshold
  - waiting on maker fill, hedge, or close longer than normal
  - repeated balance refresh failures
  - temporary risk pause without residual position
- `DANGER`
  - unhedged position
  - close failure with residual exposure
  - manual intervention state
  - stale quotes severe enough that trading should be considered unsafe

### `profit`

The operator view should show concise profit information, not the full accounting breakdown.

- during a live round: `round:<value or open>` plus `total:<value>`
- while idle: `total:<value>`

Detailed `entry`, `exit`, `gross`, `fee`, `net_quote`, and reference-price fields belong in structured round-close events, not in heartbeat lines.

### `blocked`

`blocked` should describe the single primary bottleneck. It should be one short enum-like value, not a sentence.

Recommended values:

- `none`
- `warmup`
- `maker_wait_fill`
- `hedge_wait_fill`
- `close_wait_confirm`
- `maker_stale`
- `taker_stale`
- `insufficient_liquidity`
- `balance_stale`
- `manual_intervention`

Detailed reasons remain in structured events.

## Structured event design

Structured logging should be intentionally small and event-based.

### Session events

Used for startup, warmup complete, risk pause/resume, and shutdown.

Core fields:

- `ts`
- `event`
- `maker_exchange`
- `taker_exchange`
- `symbol`
- `state`
- `reason`
- `maker_lag_ms`
- `taker_lag_ms`

### Round events

Used for `signal`, `maker_placed`, `maker_fill`, `hedge_done`, `close_done`, and `round_failed`.

Core fields:

- `ts`
- `round_id`
- `event`
- `direction`
- `qty`
- `edge_bps`
- `z`
- `expected_profit_bps`
- `latency_ms`
- `reason`

### PnL events

Used only on round completion or periodic refresh.

Core fields:

- `ts`
- `round_id`
- `net_bps`
- `gross_bps`
- `fee_bps`
- `net_quote`
- `total_quote`
- `rounds`

### Health events

Used as low-frequency machine-readable snapshots.

Core fields:

- `ts`
- `state`
- `blocked_by`
- `maker_lag_ms`
- `taker_lag_ms`
- `maker_order_age_ms`
- `position_hold_ms`

## Data to remove from the primary flow

The redesign should explicitly stop treating the following as primary logs:

- per-frame `MKT` dump lines in `run.log`
- per-tick persistence of full maker/taker BBO plus `ab/ba/mean/std/z`
- per-tick `reason_ab` / `reason_ba` logs
- schema-heavy wrappers added only for hypothetical future use

Those details are either already covered by `raw.jsonl` or should be emitted only when a state transition requires evidence.

## Minimal implementation direction

This redesign should be implemented as a simplification, not a new subsystem:

- replace per-frame `logMarketSnapshot` output with a low-frequency heartbeat aggregator
- derive `safe`, `profit`, and `blocked` from current trader and market state
- keep existing trading lifecycle logs, but normalize them into concise `EVT` style messages
- add a small structured event sink for session / round / pnl / health records
- keep `raw.jsonl` untouched except for documentation updates if needed

## Acceptance criteria

- An operator can scan `run.log` for 5 seconds and answer whether the bot is safe, profitable, and blocked.
- `run.log` no longer contains one market-detail line per orderbook update.
- A completed round still has enough structured evidence for postmortem review.
- `raw.jsonl` remains the source of truth for high-frequency orderbook evidence.
- The redesign reduces logging complexity rather than adding another parallel telemetry layer.

## Out of scope

- introducing a metrics backend, dashboard system, or external observability stack
- retaining every historical derived market metric in structured form
- redesigning strategy logic or execution policy
- replacing `raw.jsonl` with a richer market replay schema
