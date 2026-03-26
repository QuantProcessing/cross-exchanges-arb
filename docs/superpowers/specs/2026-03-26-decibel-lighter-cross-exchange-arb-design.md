# Decibel + Lighter Cross-Exchange Arbitrage Design

**Date:** 2026-03-26

## Goal

Upgrade this project to `github.com/QuantProcessing/exchanges v0.2.0`, remove hard-coded exchange construction, and validate the real-money feasibility of perp spread arbitrage with a single-position live-validation workflow. The first target pair is `DECIBEL` as maker and `LIGHTER` as taker, while the code remains generic for any two perp exchanges supported by `exchanges`.

## Product Scope

### In Scope

- Upgrade to `exchanges v0.2.0`
- Generic creation of any two perp exchanges through the `exchanges` registry
- A single-position spread arbitrage loop
- `maker -> hedge -> monitor -> close -> cooldown -> next round`
- Live-validation mode for small-size production testing
- Default validation profile: `maker=DECIBEL`, `taker=LIGHTER`
- Safety guards for in-flight orders, partial fills, hedge failure, and close failure
- Minimal automated tests for state transitions and generic exchange creation

### Out of Scope

- Multi-position portfolio trading
- Parallel strategy execution
- Backtesting engine
- Production-grade recovery for every one-leg failure mode
- Exchange-specific business logic branches in strategy code

## Design Principles

- Strategy code depends only on `exchanges.Exchange` and related unified types.
- Exchange differences are expressed through generic configuration and capability checks, not hard-coded exchange-name branches.
- Signal generation and execution are separate concerns.
- Live validation prioritizes exposure control over fee optimization.
- One verified trading loop is more valuable than a fragile always-on bot.

## Current-State Review

The current repository is a runnable MVP, but it is not yet safe enough for the intended live-validation goal.

### Positive Baseline

- The project already compiles against the pre-upgrade dependency.
- `SpreadEngine` and `Trader` are small enough to refactor in place.
- The current logic already models the intended `maker -> taker hedge` flow.

### Material Gaps

- `main.go` hard-codes `EDGEX` and `LIGHTER` constructors, which blocks generic exchange support.
- Signal generation and execution are tightly coupled, which makes execution policy hard to change safely.
- Trader state is implicit rather than modeled as an explicit execution state machine.
- Failure handling is incomplete for partial fills, hedge failure, and close-leg failure.
- There are no tests covering execution-state transitions.
- The repository has no doc-level or code-level distinction between observation, dry-run, and live-validation safety modes.

## Architecture

The system is split into five strategy-layer units.

### 1. Exchange Factory

Responsibility:

- Create adapters through the `exchanges` registry introduced in `v0.2.0`
- Accept generic runtime config:
  - exchange name
  - market type
  - quote currency
  - credentials
  - optional order mode
- Return initialized `exchanges.Exchange` values for maker and taker

This replaces hard-coded direct imports in `main.go`.

### 2. Execution Profile

Responsibility:

- Hold generic strategy-side execution defaults
- Express how the bot should trade, not what a specific exchange is

Examples:

- maker entry order type: `post-only`
- taker hedge order type: `market with slippage`
- single-position validation mode
- maker timeout
- close behavior prefers fast exit over fee optimization

The first real-money validation profile uses:

- maker exchange: `DECIBEL`
- taker exchange: `LIGHTER`
- maker entry: `post-only`
- hedge: `market + slippage protection`

This is configuration, not exchange-name branching in business logic.

### 3. Spread Engine

Responsibility:

- Subscribe to both order books
- Compute spread, rolling mean, rolling standard deviation, z-score, and fee-adjusted edge
- Produce candidate signals

The spread engine does not place orders. It only emits structured opportunities.

### 4. Execution State Machine

Responsibility:

- Own the full trading lifecycle
- Serialize all live actions
- Enforce “open one position, fully close it, then allow the next round”

States:

- `idle`
- `placing_maker`
- `waiting_fill`
- `hedging`
- `position_open`
- `closing`
- `cooldown`
- `manual_intervention`

### 5. Risk Guard

Responsibility:

- Reject actions that violate validation-mode safety rules
- Gate transitions before order placement or close actions

Checks include:

- only one active position
- no second order while one is in flight
- rate-limit cooldown
- min quantity / symbol details loaded
- stop trading after unresolved failure

## End-to-End Flow

### Startup

1. Parse runtime config.
2. Create maker and taker through the generic exchange factory.
3. Load symbol details and fee information.
4. Start order book watchers.
5. Start trader state machine.

### Open Flow

1. `SpreadEngine` emits a candidate signal.
2. Strategy validates:
   - warmed up
   - no open position
   - no in-flight order
   - not in cooldown
   - not in manual intervention mode
3. Place maker order as `post-only`.
4. Transition to `waiting_fill`.
5. On fill or partial fill, hedge the filled quantity immediately on taker with slippage protection.
6. If hedge succeeds, transition to `position_open`.

### Close Flow

Close triggers:

- z-score mean reversion
- stop-loss threshold
- max hold time

Close behavior:

1. Transition to `closing`.
2. Close both legs using fast-exit logic.
3. If both close legs succeed, clear position and enter `cooldown`.
4. After cooldown, return to `idle`.

## Failure Handling

### Maker Placement Fails

- Abort the round
- Return to `idle` or `cooldown`
- Do not place hedge orders

### Maker Times Out

- Cancel maker order
- Wait for terminal order state
- If order stayed unfilled, return to `idle`
- If a fill happened during cancel, hedge the actual filled quantity immediately

### Partial Fill

- Hedge the actual filled quantity immediately
- Do not wait for the original requested quantity

### Hedge Fails After Maker Fill

- Raise high-priority alert
- Transition to `manual_intervention`
- Block new trading rounds until human intervention clears the state

### Close Leg Fails

- Keep residual exposure state
- Alert immediately
- Block the next round
- Remain outside `idle` until exposure is resolved

### Stream / State Uncertainty

- Do not open new positions when order tracking becomes unreliable
- If a position exists, allow only conservative close behavior

## Configuration Model

The CLI/config surface should evolve from exchange-specific flags into a generic model with validation-mode defaults.

Required concepts:

- maker exchange name
- taker exchange name
- symbol
- quantity
- quote currency per side when needed
- live-validation mode toggle
- maker timeout
- cooldown
- max hold time
- slippage
- post-only buffer
- max completed rounds for validation runs

Credential loading remains exchange-specific at the config boundary, but the strategy code consumes already-created generic adapters.

## Testing Strategy

### Layer 1: Compatibility

- Project compiles with `exchanges v0.2.0`
- Generic factory can create registered perp exchanges

### Layer 2: State-Machine Tests

Add unit tests for:

- signal ignored when not `idle`
- maker timeout leads to cancel and reset
- partial fill leads to immediate hedge
- hedge failure moves to `manual_intervention`
- successful close returns to `cooldown` then `idle`
- close failure blocks next round

### Layer 3: Live Validation

Success criteria for the first production validation run:

- At least one complete real-money round trip:
  - open
  - hedge
  - close
- Event log covers every major state transition
- Filled quantities reconcile across both legs
- System returns to `idle` or `cooldown` cleanly after close
- No unknown residual position remains

If any of the following happens, validation is considered not yet proven:

- unresolved one-leg exposure
- unknown final order state
- state machine deadlock
- next round starts before the previous round is fully resolved

## Implementation Direction

The preferred implementation path is:

1. Upgrade dependency and replace hard-coded exchange creation with a generic factory.
2. Introduce explicit execution state and risk guard.
3. Preserve the current spread model, but decouple signal emission from order execution.
4. Add validation-mode constraints and tests.
5. Run live validation first with `DECIBEL` maker and `LIGHTER` taker.
6. Expand the same generic path to any two perp exchanges supported by `exchanges`.

## Acceptance Criteria

- Dependency upgraded to `exchanges v0.2.0`
- No strategy-layer exchange-name branching is required for normal execution
- Generic maker/taker perp creation works
- Trader uses explicit serialized state transitions
- Validation mode enforces one fully resolved round at a time
- Tests cover the main failure and recovery paths
- `DECIBEL + LIGHTER` can be used as the first real-money validation pair
