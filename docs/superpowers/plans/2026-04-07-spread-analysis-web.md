# Spread Analysis Web Implementation Notes

**Status:** implemented

## What shipped

The repo now ships a phase-1 spread-analysis research workbench with:

- Python standard-library backend
- SQLite-backed run + derived-point persistence
- static no-build frontend under `webapp/static/`
- single-run AB / BA overview + detail charts
- interactive threshold-duration analysis
- interactive histogram analysis
- raw upload support through the same import path as startup scanning

## Final architecture

The original React/Vite draft was not used.

The implemented shape is:

```text
raw.jsonl
  -> derived spread points
  -> SQLite
  -> /api/runs + /api/runs/{id}/points + /api/runs/{id}/analysis
  -> static browser UI
```

This preserves a dependency-light phase 1 and keeps the derived-point seam reusable for future backtest work.

## Key implementation files

- `scripts/spread_analyze.py`
- `scripts/spread_backtest.py`
- `tests/test_spread_analyze.py`
- `tests/test_spread_backtest.py`
- `tests/test_spread_web.py`
- `webapp/store.py`
- `webapp/server.py`
- `webapp/static/index.html`
- `webapp/static/styles.css`
- `webapp/static/app.js`
- `README.md`

## Current UI notes

- Large runs are downsampled before chart drawing so the browser stays responsive.
- Histograms keep the full bucket layout and label each bar with its count.
- Overview/detail are linked through the selected time window.

## Verification used during implementation

- `python3 -m unittest tests.test_spread_web tests.test_spread_analyze tests.test_spread_backtest -v`
- `node --check webapp/static/app.js`
- local `create_app(...)` smoke checks for `/`, `/app.js`, `/styles.css`, and `/api/runs`
