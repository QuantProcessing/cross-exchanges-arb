# exchanges v0.2.10 TradingAccount upgrade design

## Goal
Upgrade dependency `github.com/QuantProcessing/exchanges` to v0.2.10 and migrate all order/position handling to the new `account.TradingAccount` + orderFlow mechanism, using WS order placement (`PlaceWS`) for open/hedge/close flows.

## Current state summary
- Trader owns `exchanges.Exchange` adapters directly and wires `WatchOrders`/`WatchFills` per adapter.
- Open flow: maker post-only order placed via adapter, maker order updates drive taker hedge.
- Close flow: two market orders placed via adapter, then verify via order stream/REST fallback.
- Position verification uses `PerpExchange.FetchPositions` when needed.

## Target architecture
- Introduce `account.TradingAccount` for maker and taker.
- `TradingAccount` owns account snapshot, order/position streams, and orderFlow routing.
- Trader consumes `TradingAccount` for all order placement, order tracking, and position verification.
- Trader still holds `exchanges.Exchange` references for non-account calls (ticker/details/fees).

## Components and responsibilities
### TradingAccount
- `Start(ctx)` loads initial account snapshot and registers `WatchOrders`/`WatchPositions`.
- Maintains internal order/position state as the canonical source of truth.
- Provides subscriptions (`SubscribeOrders`, `SubscribePositions`) and flow lifecycle APIs (`PlaceWS`, `Track`).

### Trader
- Subscribes to `TradingAccount` order/position streams instead of adapter streams.
- Open flow uses `makerAccount.PlaceWS` to place maker order and track updates via `OrderFlow`.
- Hedge flow uses `takerAccount.PlaceWS` and associated flow to confirm fills.
- Close flow places both legs via `PlaceWS` and aggregates flow terminal states.
- Position validation reads from `TradingAccount.Position(symbol)` / `Positions()`.

## Data flow
### Open flow
1. Signal fires.
2. Maker `PlaceWS` with required `ClientID`.
3. OrderFlow updates: partial/fill triggers taker hedge `PlaceWS` for delta.
4. Maker terminal status -> finalize open or reset state.

### Close flow
1. Two `PlaceWS` market orders (long/short legs).
2. Wait for each order flow to reach terminal status.
3. On failure/timeout: mark manual intervention and retain residual order state.

### Position verification
- Use TradingAccount snapshots for validation instead of `FetchPositions`.

## Error handling
- TradingAccount start failure aborts run.
- `PlaceWS` failure aborts the flow and surfaces error.
- Flow timeouts or WS stream failures transition to `manual_intervention` with diagnostics.

## Tests
- Update trader tests to use TradingAccount abstraction and flow-driven updates.
- Add tests for WS order flow:
  - maker partial fill -> taker hedge
  - close dual legs terminal confirmation
  - position verification reads TradingAccount snapshot

## Out of scope
- New exchange integrations.
- Non-trading refactors unrelated to order/position flow.
