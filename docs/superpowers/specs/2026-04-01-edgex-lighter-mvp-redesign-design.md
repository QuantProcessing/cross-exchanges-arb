# EDGEX + LIGHTER Arbitrage MVP Redesign

**Date:** 2026-04-01

## Goal

Redesign the current EDGEX/LIGHTER cross-exchange arbitrage MVP so it validates whether an *executable* spread-arbitrage edge exists, rather than trading on raw top-of-book distortions.

This redesign is driven by three grounded findings:

- the current `rolling mean + rolling std + fixed z-score` gate misses persistent absolute spread regimes
- the current `LONG_TAKER_SHORT_MAKER` side is contaminated by invalid or dust-like Lighter ask quotes
- the current `LONG_MAKER_SHORT_TAKER` side can still lose money because quoted spread is not equal to executable spread

## Evidence Summary

### 1. Persistent absolute edge is being absorbed into the rolling mean

The current engine computes raw `spreadAB` / `spreadBA`, feeds them into rolling stats, then only emits signals when `z > z-open` and `spread - mean > fees + minProfit`.

See:

- [internal/spread/engine.go](/Users/dddd/Documents/GitHub/cross-exchanges-arb/internal/spread/engine.go#L174)
- [internal/spread/engine.go](/Users/dddd/Documents/GitHub/cross-exchanges-arb/internal/spread/engine.go#L227)

Observed runtime examples from [cross-arb-3-31.log](/Users/dddd/Desktop/cross-arb-3-31.log):

- `2026-03-31T11:17:08Z` → `BA=61.4bps Z=0.51`
- `2026-03-31T11:18:36Z` → `BA=81.1bps Z=1.14`
- `2026-03-31T13:50:34Z` → `BA=59.8bps Z=0.10`

These are large absolute spreads that the current gate does not treat as actionable because the regime itself has shifted.

### 2. Raw BA opportunities are mostly fake

In [spread_EDGEX_LIGHTER_BTC_20260401_032850.csv](/Users/dddd/Desktop/spread_EDGEX_LIGHTER_BTC_20260401_032850.csv):

- `spread_ba_bps > 40` appears `59,589` times
- all `59,589 / 59,589` of those rows also satisfy `taker_ask < taker_bid`
- after sanitizing `taker_ask := max(taker_ask, taker_bid)`, `BA > 20bps` drops from `80,889` rows to `0`

This means the current `LONG_TAKER_SHORT_MAKER` edge is not a real tradable edge in the observed data. It is a bad-quote edge.

### 3. Actual fills invert the quoted edge

Runtime examples:

- `R005` quoted open spread: about `+17.9bps`
- `R007` quoted open spread: about `+37.9bps`
- re-priced using actual fills, both entries become about `-13bps`

This is visible in [cross-arb-3-31.log](/Users/dddd/Desktop/cross-arb-3-31.log):

- `R005` signal and fills near `16:42:10`
- `R007` signal and fills near `16:46:06`

The key pattern is that the Lighter hedge fills tens of bps worse than the quoted bid used by the signal.

### 4. The system reasons about quotes, not executable size

The current `Signal` and `Snapshot` only carry top-of-book prices, not depth or quote size.

See:

- [internal/spread/types.go](/Users/dddd/Documents/GitHub/cross-exchanges-arb/internal/spread/types.go#L19)

This means the current model cannot distinguish:

- executable liquidity
- dust quotes
- crossed books
- stale quotes
- top-of-book that is too small for the target quantity

### 5. Slippage protection still depends on possibly polluted quotes

The hedge path uses `PlaceMarketOrderWithSlippage`:

- [internal/trading/open.go](/Users/dddd/Documents/GitHub/cross-exchanges-arb/internal/trading/open.go#L495)

That helper derives the protection price from the exchange ticker bid/ask:

- [exchange.go](/Users/dddd/go/pkg/mod/github.com/!quant!processing/exchanges@v0.2.3/exchange.go#L167)
- [base_adapter.go](/Users/dddd/go/pkg/mod/github.com/!quant!processing/exchanges@v0.2.3/base_adapter.go#L248)

If the quote is wrong, the protection price is wrong.

## What To Learn From yourQuantGuy

The two references are useful, but not in the naive sense of “switch to moving average and rolling std and the strategy becomes profitable.”

Relevant points:

- [yourQuantGuy post](https://x.com/yourQuantGuy/status/1994346643903201311): explicitly frames `moving average spread + rolling std` as one optimization for *dynamic* open/close logic, not as the entire strategy
- the same post also highlights order-book freshness, websocket vs REST consistency, exchange health checks, and dynamic open/close behavior
- [yourQuantGuy article share](https://x.com/yourQuantGuy/status/1989511712186278020): stresses order-book monitoring, data cleaning, execution architecture, and expected-vs-actual execution deviation

The correct adaptation for this repo is:

- first fix the market-data surface so it represents tradable prices
- then use moving average / rolling std on that sanitized executable spread
- use the stat model as a dynamic thresholding tool, not as the sole source of truth

## Design Principles

- Prefer executable spread over quoted spread
- Treat invalid or suspicious quotes as missing data, not opportunity
- Separate signal ranking from execution approval
- Disable directions that fail data-quality validation
- Let dynamic thresholds shape entry and exit, but only after liquidity checks pass
- Measure expected-vs-actual execution deviation continuously and pause on drift

## New Strategy Model

The redesigned MVP should operate in four layers.

### 1. Quote Sanitation Layer

Purpose:

- reject or repair obviously invalid market data before strategy logic sees it

Rules for each exchange snapshot:

- reject if `bid <= 0` or `ask <= 0`
- reject if `ask < bid`
- reject if quote age exceeds freshness threshold
- reject if top-of-book quantity is below a configurable minimum
- reject if price jump from previous valid snapshot exceeds a sanity threshold without confirmation
- optionally compare websocket BBO to recent REST/ticker references and mark the stream degraded on persistent mismatch

For the current MVP:

- preserve both directions
- require each direction to pass quote sanitation and direction-specific executable checks before it can generate a signal

### 2. Executable Spread Layer

Purpose:

- convert market data into a tradeable edge estimate for the target order quantity

Definition:

- executable buy price = quantity-aware ask-side effective price
- executable sell price = quantity-aware bid-side effective price
- executable open spread = `sell leg effective price - buy leg effective price`

If depth is not yet available from both venues, the MVP fallback is:

- require top-of-book size to exceed `qty * liquidityMultiplier`
- if size is insufficient, treat the quote as non-executable

This is weaker than full depth-aware VWAP, but much safer than raw BBO.

### 3. Dynamic Entry / Exit Layer

Purpose:

- use historical spread behavior without letting the rolling mean redefine all absolute edge as “normal”

Open logic:

- compute rolling moving average and rolling std on **sanitized executable spread**
- compute standardized distance from the moving average
- require both:
  - dynamic regime condition, such as `spread > movingAverage + k * rollingStd`
  - hard absolute edge condition, such as `spread > fees + impactBuffer + latencyBuffer + minNetOpenBps`

Close logic:

- do not close only because quote-side Z reverted
- close when executable exit edge implies:
  - target profit locked
  - stop-loss breached
  - expected hold efficiency is no longer attractive
  - maximum hold time exceeded

This is the key synthesis:

- dynamic thresholds from moving average / rolling std
- hard guardrails from executable net edge

### 4. Execution Quality Layer

Purpose:

- verify that the realized execution matches the modeled assumptions

Track for every round:

- signal spread
- approved executable spread
- maker placed price
- maker fill price
- taker hedge quote at submit
- taker hedge actual fill
- quote-to-fill deviation in bps
- full round realized net bps

Kill switch:

- if `expected-vs-actual` deviation exceeds threshold over `N` trades or `M` minutes, pause trading

This directly mirrors the operational advice from yourQuantGuy’s article.

## MVP Scope

### In Scope

- preserve both directions after abnormal-quote filtering
- add quote sanitation for invalid/crossed/stale Lighter data
- gate entries on sanitized executable open edge
- add dynamic open/close thresholds using moving average + rolling std on sanitized spread
- add expected-vs-actual execution deviation monitoring
- preserve the current single-position, one-round-at-a-time safety model

### Out of Scope

- multi-account routing
- true multi-venue smart order routing
- advanced cross-venue maker/maker execution patterns
- portfolio-level capital allocation
- cloud architecture, bots, and production telemetry beyond what is needed for MVP validation

## Concrete MVP Rules

### Direction Policy

- keep both `LONG_MAKER_SHORT_TAKER` and `LONG_TAKER_SHORT_MAKER`
- require each direction to independently pass sanitation, freshness, and executable-quantity checks

### Signal Approval Policy

An open candidate is valid only if all conditions pass:

1. both exchange snapshots are fresh
2. both sides pass quote sanitation
3. target quantity is executable at the top level or configured minimum depth proxy
4. sanitized executable spread exceeds:
   - round-trip fees
   - configured market impact buffer
   - configured latency buffer
   - configured minimum net open bps
5. executable spread also exceeds the dynamic threshold:
   - `movingAverage + entrySigma * rollingStd`

### Exit Policy

Exit when any one of the following is true:

1. executable close spread locks target net profit
2. executable close spread breaches stop-loss
3. expected edge decays below hold threshold for too long
4. max hold time is exceeded
5. execution-quality monitor pauses the strategy

## Acceptance Criteria

The redesign is successful only if all of the following are true:

1. Raw crossed or inverted Lighter quotes no longer generate strategy signals.
2. `LONG_TAKER_SHORT_MAKER` no longer trades on abnormal quotes; it remains available when sanitized Lighter ask data and executable-quantity checks pass.
3. Entry approval uses a quantity-aware executable edge model rather than raw BBO spread alone.
4. Moving average + rolling std operate on sanitized executable spread, not raw spread.
5. The strategy records expected-vs-actual hedge deviation for every executed round.
6. The strategy can pause itself when execution deviation becomes abnormal.
7. Close decisions use executable edge or realized-risk conditions, not quote-Z alone.

## Recommendation

The next implementation step should not be “retune z-open/z-close.”

It should be:

1. sanitize quotes
2. filter abnormal direction-specific quotes instead of globally disabling a side
3. redefine spread as executable spread
4. then reintroduce moving-average / rolling-std as dynamic thresholds on top of that cleaner edge

That is the part of yourQuantGuy’s logic that is genuinely helpful to this MVP.
