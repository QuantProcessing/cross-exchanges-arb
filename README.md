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
3. When Z-Score exceeds `--z-open`, signals a trade if net profit > fees + `--min-profit`
4. **Trader** places a **post-only maker order** on the maker exchange
5. On fill, immediately **hedges with a market order** on the taker exchange
6. Monitors position for **close** (Z < `--z-close`), **stop-loss** (Z < `--z-stop`), or **timeout** (`--max-hold`)

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

### Run Modes

**Phase 0 — Observe** (collect spread data only, no trading):
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --observe-only
```
Outputs a CSV file for offline analysis.

**Phase 1 — Dry Run** (simulate signals, no real orders):
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m --dry-run
```

**Phase 2 — Live Validation**:
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m \
  --maker-timeout 15s --max-rounds 1 --live-validate=true
```

Live validation mode is intentionally constrained:

- one active round at a time
- maker order timeout and settlement-aware cancel flow
- partial maker fills hedge immediately on the taker leg
- successful close enters cooldown before the next round
- unresolved hedge/close failures move the trader into `manual_intervention`

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
| `--z-open` | `2.0` | Z-Score threshold to open position |
| `--z-close` | `0.5` | Z-Score threshold to close (take profit) |
| `--z-stop` | `-1.0` | Z-Score for stop-loss |
| `--window` | `500` | Rolling window size in ticks |
| `--min-profit` | `1.0` | Minimum net profit in BPS after fees |
| `--warmup-ticks` | `200` | Minimum ticks before trading |
| `--warmup-duration` | `3m` | Minimum wall-clock warmup before trading |
| `--cooldown` | `5s` | Cooldown between trades |
| `--max-hold` | `30m` | Max position hold time |
| `--slippage` | `0.002` | Slippage tolerance (0.2%) |
| `--maker-timeout` | `15s` | Maker order timeout before cancel/reset |
| `--max-rounds` | `1` | Max completed rounds in live validation mode |
| `--live-validate` | `true` | Enable live-validation safeguards |
| `--dry-run` | `false` | Simulate only |
| `--observe-only` | `false` | Collect data only, output CSV |

## Deployment

### Build

```bash
# Cross-compile for Linux server
GOOS=linux GOARCH=amd64 go build -o cross-arb .
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
```

## Project Structure

```
├── main.go                  # Thin CLI entrypoint
├── internal/
│   ├── app/                # Startup wiring, runtime orchestration
│   ├── config/             # CLI parsing and config validation
│   ├── exchange/           # Exchange registry wiring and credential mapping
│   ├── spread/             # Rolling stats, BBO monitoring, signal generation
│   └── trading/            # Trader state machine, execution, PnL tracking
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
