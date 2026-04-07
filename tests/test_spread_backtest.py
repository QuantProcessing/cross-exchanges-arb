from __future__ import annotations

import unittest
from datetime import datetime, timedelta, timezone
from decimal import Decimal

from scripts.spread_analyze import ReplayPoint
from scripts.spread_backtest import BacktestConfig, backtest_points


def make_point(ts: datetime, spread_ab_bps: float, spread_ba_bps: float) -> ReplayPoint:
    return ReplayPoint(
        ts_local=ts,
        symbol="BTC",
        maker_exchange="EDGEX",
        taker_exchange="LIGHTER",
        maker_bid=Decimal("0"),
        maker_ask=Decimal("0"),
        taker_bid=Decimal("0"),
        taker_ask=Decimal("0"),
        spread_ab_bps=spread_ab_bps,
        spread_ba_bps=spread_ba_bps,
    )


class SpreadBacktestTest(unittest.TestCase):
    def test_backtest_opens_and_closes_ab_trade(self) -> None:
        base = datetime(2026, 4, 6, 0, 0, 0, tzinfo=timezone.utc)
        points = [
            make_point(base + timedelta(seconds=0), 1.0, -3.0),
            make_point(base + timedelta(seconds=1), 1.0, -3.0),
            make_point(base + timedelta(seconds=2), 1.0, -3.0),
            make_point(base + timedelta(seconds=3), 8.0, -6.0),
            make_point(base + timedelta(seconds=4), 2.0, 1.0),
        ]
        config = BacktestConfig(
            window=3,
            open_buffer_bps=5.0,
            close_buffer_bps=1.0,
            stop_buffer_bps=-2.0,
            fee_bps=2.0,
            max_hold_seconds=30,
        )

        result = backtest_points(points, config)

        self.assertEqual(result.total_trades, 1)
        self.assertEqual(result.winning_trades, 1)
        self.assertAlmostEqual(result.total_net_pnl_bps, 7.0)
        self.assertAlmostEqual(result.average_net_pnl_bps, 7.0)
        self.assertEqual(result.total_hold_ms, 1000)
        self.assertEqual(result.ab.trade_count, 1)
        self.assertEqual(result.ab.win_count, 1)
        self.assertAlmostEqual(result.ab.total_net_pnl_bps, 7.0)
        self.assertEqual(result.ba.trade_count, 0)
        self.assertEqual(result.trades[0].direction, "AB")
        self.assertEqual(result.trades[0].exit_reason, "reversion")
        self.assertAlmostEqual(result.trades[0].gross_pnl_bps, 9.0)

    def test_backtest_opens_and_closes_ba_trade(self) -> None:
        base = datetime(2026, 4, 6, 1, 0, 0, tzinfo=timezone.utc)
        points = [
            make_point(base + timedelta(seconds=0), -2.0, 1.0),
            make_point(base + timedelta(seconds=1), -2.0, 1.0),
            make_point(base + timedelta(seconds=2), -2.0, 1.0),
            make_point(base + timedelta(seconds=3), -6.0, 9.0),
            make_point(base + timedelta(seconds=4), 1.5, 2.0),
        ]
        config = BacktestConfig(
            window=3,
            open_buffer_bps=5.0,
            close_buffer_bps=1.0,
            stop_buffer_bps=-2.0,
            fee_bps=1.5,
            max_hold_seconds=30,
        )

        result = backtest_points(points, config)

        self.assertEqual(result.total_trades, 1)
        self.assertEqual(result.winning_trades, 1)
        self.assertAlmostEqual(result.total_net_pnl_bps, 9.0)
        self.assertEqual(result.ba.trade_count, 1)
        self.assertEqual(result.ba.win_count, 1)
        self.assertAlmostEqual(result.ba.total_net_pnl_bps, 9.0)
        self.assertEqual(result.trades[0].direction, "BA")
        self.assertEqual(result.trades[0].hold_ms, 1000)
        self.assertAlmostEqual(result.trades[0].gross_pnl_bps, 10.5)


if __name__ == "__main__":
    unittest.main()
