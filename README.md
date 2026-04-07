# Cross-Exchange Spread Arbitrage

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A **maker-taker arbitrage bot** that captures spread discrepancies between two perpetual-futures exchanges. It monitors BBO (best bid/offer) in real-time, detects statistical deviations using a **Z-Score mean-reversion model**, and executes hedged trades by placing a maker order on one exchange and immediately hedging on the other.

[🇨🇳 中文文档](README_CN.md)

## How It Works

```
Exchange A (Maker)         Exchange B (Taker)
     │                          │
     └──── BBO WebSocket ───────┘
                │
        ┌───────▼───────┐
        │ Spread Engine  │  rolling window → μ, σ → Z-Score
        │  Z-Score Model │  signal when |Z| > threshold
        └───────┬───────┘
                │ signal
        ┌───────▼───────┐
        │    Trader      │  post-only limit on Maker
        │  Maker-Taker   │  on fill → market hedge on Taker
        └───────┬───────┘
                │
        ┌───────▼───────┐
        │   Telegram     │  trade / error notifications
        └───────────────┘
```

1. **Spread Engine** subscribes to both exchange order books via WebSocket
2. Computes rolling mean (μ) and standard deviation (σ) over a configurable window
3. Sanitizes top-of-book quotes, checks executable spread for the target quantity, then opens only when spread clears both hard net-edge buffers and the dynamic `mean + z-open * rolling std` threshold
4. **Trader** places a **post-only maker order** on the maker exchange
5. On fill, immediately **hedges with a market order** on the taker exchange
6. Monitors position for **close** when sanitized executable spread reverts below the dynamic `mean + z-close * rolling std` threshold, on **stop-loss** (`--z-stop`), or on **timeout** (`--max-hold`)

## Exchange Support

| Exchange | Role | Notes |
|:---------|:-----|:------|
| [Decibel](https://www.decibel.exchange/) | Maker or Taker | Perp futures, tested first-class in the current validation flow |
| [EdgeX](https://www.edgex.exchange/) | Maker or Taker | Perp futures, requires API key + account ID |
| [Hyperliquid](https://hyperliquid.xyz/) | Maker or Taker | Perp futures, supported through registry-based exchange wiring |
| [Lighter](https://lighter.xyz/) | Maker or Taker | Perp futures, zero taker fees recommended as taker |

Built on the [exchanges](https://github.com/QuantProcessing/exchanges) `v0.2.0` unified SDK. This repo now creates exchanges through the SDK registry instead of hard-coded constructors, so the strategy layer stays on top of `exchanges.Exchange`.

Current repo wiring includes ready-to-run credential mapping for `DECIBEL`, `LIGHTER`, `EDGEX`, and `HYPERLIQUID`. The execution model is intended for any two perp exchanges exposed through `exchanges`, but the validated path in this repo is currently `DECIBEL` maker + `LIGHTER` taker.

## Quick Start

### Prerequisites

- Go 1.26+
- API credentials for both exchanges

### Setup

```bash
git clone https://github.com/QuantProcessing/cross-exchanges-arb.git
cd cross-exchanges-arb

# Configure credentials
cp .env.example .env
# Edit .env with your API keys
```

### Run

The runtime now executes directly in `live`; there is no dry-run or observe-only mode selector in the CLI.

Each run creates an isolated artifact directory under `logs/<timestamp>_<maker>_<taker>_<symbol>/`:

- `run.log`: operator-facing runtime log with low-frequency `STAT` heartbeats plus `EVT` lifecycle lines for trading and PnL events
- `events.jsonl`: low-volume structured event stream for `session`, `round`, `pnl`, and `health` events
- `raw.jsonl`: raw single-side orderbook stream for maker and taker, written as separate records in one file

### `raw.jsonl` Schema

`raw.jsonl` writes one JSON object per exchange-side orderbook snapshot. It is not a unified maker+taker record: maker updates and taker updates are emitted as separate lines in the same file.

Top-level fields:

| Field | Meaning |
| --- | --- |
| `ts_local` | Local wall-clock time when this raw record was written. |
| `side` | Which leg this record belongs to: `maker` or `taker`. |
| `exchange` | Exchange name for this record. |
| `symbol` | Base symbol being tracked, for example `BTC`. |
| `exchange_ts` | Timestamp carried by the local orderbook snapshot. When the adapter has no native server timestamp, it may fall back to a locally stamped receive time. EdgeX currently falls into this best-effort category. |
| `quote_lag_ms` | Best-effort quote lag in milliseconds, computed as `ts_local - exchange_ts` when `exchange_ts` exists. |
| `bids` | Bid levels included in this raw snapshot. |
| `asks` | Ask levels included in this raw snapshot. |

Nested book fields:

| Field | Meaning |
| --- | --- |
| `bids[].price` | Price for one bid level. |
| `bids[].qty` | Quantity available at that bid level. |
| `asks[].price` | Price for one ask level. |
| `asks[].qty` | Quantity available at that ask level. |

Raw stream notes:

- `raw.jsonl` intentionally omits `schema_version`.
- The runtime currently records top-2 levels from each side.
- Because maker and taker are emitted separately, downstream tooling must merge them if it wants a unified cross-exchange view.

### `run.log`

`run.log` is optimized for the operator on call:

- `STAT ...` lines answer whether the bot is safe, profitable, and blocked
- `EVT ...` lines capture important lifecycle transitions such as signal, maker placement, hedge completion/failure, close completion/failure, manual intervention, and PnL updates
- the runtime no longer writes one market-detail line per orderbook update

### `events.jsonl`

`events.jsonl` is the replay-oriented structured stream. It stays intentionally small and only records:

- `session` events such as run start / stop
- `health` events emitted at the same low frequency as `STAT` heartbeats
- `round` events for important trading lifecycle transitions
- `pnl` events for periodic balance refreshes and realized round summaries

**Run**:
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m \
  --maker-timeout 15s --max-rounds 1
```

The runtime is intentionally constrained:

- one active round at a time
- maker order timeout and settlement-aware cancel flow
- partial maker fills hedge immediately on the taker leg
- successful close enters cooldown before the next round
- unresolved hedge/close failures move the trader into `manual_intervention`
- every opened round logs a structured recap with signal / maker / fill / hedge BBO snapshots and stage timings

## Configuration

### Environment Variables

```bash
# Exchange credentials
EXCHANGES_DECIBEL_API_KEY=...
EXCHANGES_DECIBEL_PRIVATE_KEY=...
EXCHANGES_DECIBEL_SUBACCOUNT_ADDR=...
EXCHANGES_EDGEX_PRIVATE_KEY=...
EXCHANGES_EDGEX_ACCOUNT_ID=...
EXCHANGES_LIGHTER_PRIVATE_KEY=...
EXCHANGES_LIGHTER_ACCOUNT_INDEX=...
EXCHANGES_LIGHTER_KEY_INDEX=...
EXCHANGES_LIGHTER_RO_TOKEN=...

# Telegram notifications (optional)
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=...
```

### CLI Flags

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--maker` | `EDGEX` | Maker exchange (places limit orders) |
| `--taker` | `LIGHTER` | Taker exchange (market order hedging) |
| `--maker-quote-currency` | `` | Optional maker quote currency override |
| `--taker-quote-currency` | `` | Optional taker quote currency override |
| `--symbol` | `BTC` | Trading symbol (base currency) |
| `--qty` | `0.001` | Order quantity in base currency |
| `--z-open` | `2.0` | Dynamic open threshold multiplier on sanitized executable spread |
| `--z-close` | `0.5` | Dynamic close threshold multiplier on sanitized executable spread |
| `--z-stop` | `-1.0` | Z-Score for stop-loss |
| `--window` | `500` | Rolling window size in ticks |
| `--min-profit` | `1.0` | Minimum net profit in BPS after fees |
| `--impact-buffer-bps` | `0` | Extra BPS buffer reserved for market impact |
| `--latency-buffer-bps` | `0` | Extra BPS buffer reserved for latency / quote drift |
| `--warmup-ticks` | `200` | Minimum ticks before trading |
| `--warmup-duration` | `3m` | Minimum wall-clock warmup before trading |
| `--cooldown` | `5s` | Cooldown between trades |
| `--max-hold` | `30m` | Max position hold time |
| `--slippage` | `0.002` | Slippage tolerance (0.2%) |
| `--maker-timeout` | `15s` | Maker order timeout before cancel/reset |
| `--max-quote-age` | `1.5s` | Max accepted quote age before the signal is treated as stale |
| `--max-rounds` | `1` | Max completed rounds before the runtime stops opening new rounds |

## Deployment

### Build

```bash
# Build Linux amd64 binary into dist/
bash scripts/build-linux-amd64.sh

# Output
# dist/cross-arb-linux-amd64
```

### PM2

```bash
# Upload binary + .env + ecosystem.config.js to server
pm2 start ecosystem.config.js

# Operations
pm2 logs cross-arb
pm2 restart cross-arb
pm2 stop cross-arb
```

## Testing

```bash
go test ./...
python3 -m unittest tests.test_spread_analyze tests.test_spread_web tests.test_spread_backtest -v
```

## Spread Analysis Assistant Site

This repo includes a Python standard-library spread-analysis site for turning `raw.jsonl` logs into a single-run research workbench.

Phase 1 focuses on:

- AB / BA spread overview for whole-run anomaly scanning
- linked detail inspection for a selected time window
- interactive threshold-duration analysis
- interactive histogram distribution analysis with per-bucket count labels

### Run the site

```bash
python3 -m webapp.server \
  --host 127.0.0.1 \
  --port 8000 \
  --db-path data/spread-analysis.db \
  --logs-dir logs \
  --upload-dir data/uploads
```

Then open [http://127.0.0.1:8000](http://127.0.0.1:8000).

On startup the backend scans `logs/**/raw.jsonl`, imports normalized spread points into SQLite, and serves a static browser UI from `webapp/static/`.

### API surface

- `GET /api/runs`
- `GET /api/runs/<run_id>`
- `GET /api/runs/<run_id>/points`
- `GET /api/runs/<run_id>/analysis?threshold_bps=10&bucket_size_bps=1`
- `POST /api/uploads` with the raw file body and `X-Filename: your-file.jsonl`

`/api/runs/<run_id>/analysis` recomputes research metrics from persisted spread points rather than rereading `raw.jsonl` on every request. The response includes:

- direction-level summary metrics for AB and BA
- threshold intervals with start/end timestamp, duration, and peak spread
- histogram buckets at the requested bucket size

### Current UI behavior

- large runs are downsampled for chart rendering so the overview/detail charts stay responsive
- histogram charts preserve the requested full bucket layout and render the count above each bar
- uploads are optional; existing `logs/**/raw.jsonl` runs appear automatically after startup ingestion

### Future backtest seam

The site persists normalized derived spread points per run:

- `ts_local`
- `symbol`
- `maker_exchange`
- `taker_exchange`
- `spread_ab_bps`
- `spread_ba_bps`

That stored point contract is intended to be the bridge into future browser-triggered backtest workflows, so later backtest work can reuse the same imported dataset instead of rebuilding ingestion from scratch.

## Project Structure

```
├── main.go                  # Thin CLI entrypoint
├── internal/
│   ├── app/                # Startup wiring, runtime orchestration
│   ├── config/             # CLI parsing and config validation
│   ├── exchange/           # Exchange registry wiring and credential mapping
│   ├── spread/             # Rolling stats, BBO monitoring, signal generation
│   └── trading/            # Trader state machine, execution, PnL tracking
├── webapp/                 # Python stdlib spread-analysis backend + static UI
├── ecosystem.config.js     # PM2 process manager config
├── .env.example            # Environment variable template
└── .gitignore
```

## Algorithm Details

The Z-Score model tracks the price spread between two exchanges:

```
spread = exchange_A_price - exchange_B_price
Z = (spread - μ) / σ
```

Where μ and σ are computed over a rolling window of recent ticks. The model assumes spreads are **mean-reverting** — large deviations from the mean tend to revert.

**Fee-aware filtering**: Signals are only emitted when `expectedProfit > roundTripFees + minProfitBps`, where round-trip fees account for maker fees on the maker exchange and taker fees on the taker exchange (×2 for open + close).

## Limitations

> ⚠️ **This is an MVP (Minimum Viable Product) for algorithm validation.** It is a working prototype, not production-grade software.

The following are still **not** production-ready in the current version:

- **Rate limiting** — No throttling of API requests; rapid signals may trigger exchange rate limits causing order rejections
- **Retry logic** — Failed orders are not retried
- **Automatic recovery from one-leg failures** — The trader blocks in `manual_intervention`; it does not autonomously repair a broken hedge/close
- **Multi-position** — Only one position at a time
- **Dynamic fee refresh** — Fees are fetched once at startup
- **Reconnection** — WebSocket disconnects are not gracefully recovered
- **Generic exchange option plumbing** — The strategy is registry-based, but this repo only prewires credential/env mapping for a small set of exchanges

## License

MIT
