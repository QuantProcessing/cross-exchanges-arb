import tempfile
import textwrap
import unittest
from datetime import datetime, timedelta, timezone
from io import StringIO
from pathlib import Path
from unittest import mock

from scripts.spread_analyze import (
    analyze_points,
    main,
    replay_spreads,
    resolve_raw_path,
    summarize_raw_path,
    summarize_spreads,
)


class SpreadAnalyzeTest(unittest.TestCase):
    def test_replay_and_summarize_spreads(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            raw_path = Path(tmpdir) / "raw.jsonl"
            raw_path.write_text(
                textwrap.dedent(
                    """\
                    {"ts_local":"2026-04-06T09:42:39.000000Z","side":"maker","exchange":"EDGEX","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"5"}],"asks":[{"price":"101","qty":"6"}]}
                    {"ts_local":"2026-04-06T09:42:39.100000Z","side":"taker","exchange":"LIGHTER","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.100000+08:00","quote_lag_ms":0,"bids":[{"price":"102","qty":"7"}],"asks":[{"price":"103","qty":"8"}]}
                    {"ts_local":"2026-04-06T09:42:39.200000Z","side":"maker","exchange":"EDGEX","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.200000+08:00","quote_lag_ms":0,"bids":[{"price":"103","qty":"5"}],"asks":[{"price":"104","qty":"6"}]}
                    {"ts_local":"2026-04-06T09:42:39.300000Z","side":"taker","exchange":"LIGHTER","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.300000+08:00","quote_lag_ms":0,"bids":[{"price":"101","qty":"7"}],"asks":[{"price":"102","qty":"8"}]}
                    """
                ),
                encoding="utf-8",
            )

            points = replay_spreads(raw_path)
            summary = summarize_spreads(points)

        self.assertEqual(len(points), 3)
        self.assertAlmostEqual(points[0].spread_ab_bps, 98.52216748768473)
        self.assertAlmostEqual(points[1].spread_ab_bps, -194.1747572815534)
        self.assertAlmostEqual(points[2].spread_ab_bps, -292.6829268292683)
        self.assertAlmostEqual(summary.ab_max.spread_ab_bps, 98.52216748768473)
        self.assertAlmostEqual(summary.ab_min.spread_ab_bps, -292.6829268292683)
        self.assertAlmostEqual(summary.ba_max.spread_ba_bps, 97.5609756097561)
        self.assertAlmostEqual(summary.ba_min.spread_ba_bps, -295.5665024630542)
        self.assertEqual(summary.ab_max.ts_local.isoformat(), "2026-04-06T09:42:39.100000+00:00")
        self.assertEqual(summary.ab_min.ts_local.isoformat(), "2026-04-06T09:42:39.300000+00:00")

    def test_resolve_raw_path_uses_latest_logs_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            logs_dir = Path(tmpdir) / "logs"
            older_run = logs_dir / "20260405_113231_EDGEX_LIGHTER_BTC"
            latest_run = logs_dir / "20260406_094223_EDGEX_LIGHTER_ETH"
            older_run.mkdir(parents=True)
            latest_run.mkdir(parents=True)
            (older_run / "raw.jsonl").write_text("", encoding="utf-8")
            (latest_run / "raw.jsonl").write_text("", encoding="utf-8")

            resolved = resolve_raw_path(logs_dir)

        self.assertEqual(resolved, latest_run / "raw.jsonl")

    def test_summarize_raw_path_streams_without_reading_whole_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            raw_path = Path(tmpdir) / "raw.jsonl"
            raw_path.write_text(
                textwrap.dedent(
                    """\
                    {"ts_local":"2026-04-06T09:42:39.000000Z","side":"maker","exchange":"EDGEX","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"5"}],"asks":[{"price":"101","qty":"6"}]}
                    {"ts_local":"2026-04-06T09:42:39.100000Z","side":"taker","exchange":"LIGHTER","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.100000+08:00","quote_lag_ms":0,"bids":[{"price":"102","qty":"7"}],"asks":[{"price":"103","qty":"8"}]}
                    """
                ),
                encoding="utf-8",
            )

            with mock.patch.object(Path, "read_text", side_effect=AssertionError("read_text should not be used")):
                summary = summarize_raw_path(raw_path)

        self.assertEqual(summary.points, 1)
        self.assertAlmostEqual(summary.ab_max.spread_ab_bps, 98.52216748768473)

    def test_main_prints_progress_with_current_line_result(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            raw_path = Path(tmpdir) / "raw.jsonl"
            raw_path.write_text(
                textwrap.dedent(
                    """\
                    {"ts_local":"2026-04-06T09:42:39.000000Z","side":"maker","exchange":"EDGEX","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"5"}],"asks":[{"price":"101","qty":"6"}]}
                    {"ts_local":"2026-04-06T09:42:39.100000Z","side":"taker","exchange":"LIGHTER","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.100000+08:00","quote_lag_ms":0,"bids":[{"price":"102","qty":"7"}],"asks":[{"price":"103","qty":"8"}]}
                    """
                ),
                encoding="utf-8",
            )
            stdout = StringIO()

            exit_code = main(
                [str(raw_path), "--progress-every", "1"],
                stdout=stdout,
            )

        output = stdout.getvalue()
        self.assertEqual(exit_code, 0)
        self.assertIn("progress:", output)
        self.assertIn("line=2", output)
        self.assertIn("current_ab=98.52bps", output)
        self.assertIn("current_ba=-295.57bps", output)

    def test_analyze_points_counts_bidirectional_threshold_opportunities(self) -> None:
        base = datetime(2026, 4, 6, 0, 0, 0, tzinfo=timezone.utc)
        with tempfile.TemporaryDirectory() as tmpdir:
            raw_path = Path(tmpdir) / "raw.jsonl"
            raw_path.write_text(
                textwrap.dedent(
                    """\
                    {"ts_local":"2026-04-06T00:00:00.000000Z","side":"maker","exchange":"EDGEX","symbol":"BTC","exchange_ts":"2026-04-06T08:00:00+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"1"}],"asks":[{"price":"100.5","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:01.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:01+08:00","quote_lag_ms":0,"bids":[{"price":"101.0","qty":"1"}],"asks":[{"price":"101.2","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:02.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:02+08:00","quote_lag_ms":0,"bids":[{"price":"101.1","qty":"1"}],"asks":[{"price":"101.3","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:03.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:03+08:00","quote_lag_ms":0,"bids":[{"price":"100.4","qty":"1"}],"asks":[{"price":"100.6","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:04.000000Z","side":"maker","exchange":"EDGEX","symbol":"BTC","exchange_ts":"2026-04-06T08:00:04+08:00","quote_lag_ms":0,"bids":[{"price":"101.2","qty":"1"}],"asks":[{"price":"101.6","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:05.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:05+08:00","quote_lag_ms":0,"bids":[{"price":"100.8","qty":"1"}],"asks":[{"price":"100.9","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:06.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:06+08:00","quote_lag_ms":0,"bids":[{"price":"101.4","qty":"1"}],"asks":[{"price":"101.7","qty":"1"}]}
                    """
                ),
                encoding="utf-8",
            )

            points = replay_spreads(raw_path)
            analysis = analyze_points(points, threshold_bps=5.0, bucket_size_bps=1.0)

        self.assertEqual(analysis.ab.samples, 6)
        self.assertEqual(analysis.ab.opportunity_count, 1)
        self.assertEqual(analysis.ab.above_threshold_samples, 2)
        self.assertEqual(analysis.ab.total_opportunity_duration_ms, 1000)
        self.assertEqual(analysis.ab.longest_opportunity_duration_ms, 1000)
        self.assertEqual(len(analysis.ab.intervals), 1)
        self.assertEqual(analysis.ab.intervals[0].duration_ms, 1000)
        self.assertEqual(analysis.ab.intervals[0].start_ts_local, base + timedelta(seconds=1))
        self.assertEqual(analysis.ab.intervals[0].end_ts_local, base + timedelta(seconds=2))
        self.assertEqual(analysis.ba.opportunity_count, 1)
        self.assertEqual(analysis.ba.above_threshold_samples, 2)
        self.assertEqual(analysis.ba.total_opportunity_duration_ms, 1000)
        self.assertEqual(len(analysis.ba.intervals), 1)
        self.assertEqual(analysis.ba.intervals[0].duration_ms, 1000)
        self.assertGreater(analysis.ab.max_spread_bps, 40)
        self.assertGreater(analysis.ba.max_spread_bps, 20)

    def test_main_prints_bidirectional_analysis_summary(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            raw_path = Path(tmpdir) / "raw.jsonl"
            raw_path.write_text(
                textwrap.dedent(
                    """\
                    {"ts_local":"2026-04-06T00:00:00.000000Z","side":"maker","exchange":"EDGEX","symbol":"BTC","exchange_ts":"2026-04-06T08:00:00+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"1"}],"asks":[{"price":"100.5","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:01.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:01+08:00","quote_lag_ms":0,"bids":[{"price":"101.0","qty":"1"}],"asks":[{"price":"101.2","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:02.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:02+08:00","quote_lag_ms":0,"bids":[{"price":"101.1","qty":"1"}],"asks":[{"price":"101.3","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:03.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:03+08:00","quote_lag_ms":0,"bids":[{"price":"100.4","qty":"1"}],"asks":[{"price":"100.6","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:04.000000Z","side":"maker","exchange":"EDGEX","symbol":"BTC","exchange_ts":"2026-04-06T08:00:04+08:00","quote_lag_ms":0,"bids":[{"price":"101.2","qty":"1"}],"asks":[{"price":"101.6","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:05.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:05+08:00","quote_lag_ms":0,"bids":[{"price":"100.8","qty":"1"}],"asks":[{"price":"100.9","qty":"1"}]}
                    {"ts_local":"2026-04-06T00:00:06.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:06+08:00","quote_lag_ms":0,"bids":[{"price":"101.4","qty":"1"}],"asks":[{"price":"101.7","qty":"1"}]}
                    """
                ),
                encoding="utf-8",
            )
            stdout = StringIO()

            exit_code = main(
                [str(raw_path), "--threshold", "5", "--bucket-size", "1", "--progress-every", "0"],
                stdout=stdout,
            )

        output = stdout.getvalue()
        self.assertEqual(exit_code, 0)
        self.assertIn("Distribution", output)
        self.assertIn("AB opportunities > 5.00 bps", output)
        self.assertIn("BA opportunities > 5.00 bps", output)
        self.assertIn("occurrences: 1", output)


if __name__ == "__main__":
    unittest.main()
