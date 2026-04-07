import json
import tempfile
import textwrap
import unittest
from pathlib import Path

from webapp.server import create_app
from webapp.store import SpreadStore, scan_logs


SAMPLE_RAW_JSONL = textwrap.dedent(
    """\
    {"ts_local":"2026-04-06T09:42:39.000000Z","side":"maker","exchange":"EDGEX","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"5"}],"asks":[{"price":"101","qty":"6"}]}
    {"ts_local":"2026-04-06T09:42:39.100000Z","side":"taker","exchange":"LIGHTER","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.100000+08:00","quote_lag_ms":0,"bids":[{"price":"102","qty":"7"}],"asks":[{"price":"103","qty":"8"}]}
    {"ts_local":"2026-04-06T09:42:39.200000Z","side":"maker","exchange":"EDGEX","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.200000+08:00","quote_lag_ms":0,"bids":[{"price":"103","qty":"5"}],"asks":[{"price":"104","qty":"6"}]}
    {"ts_local":"2026-04-06T09:42:39.300000Z","side":"taker","exchange":"LIGHTER","symbol":"ETH","exchange_ts":"2026-04-06T17:42:39.300000+08:00","quote_lag_ms":0,"bids":[{"price":"101","qty":"7"}],"asks":[{"price":"102","qty":"8"}]}
    """
)


BAD_RAW_JSONL = '{"ts_local":"2026-04-06T09:42:39.000000Z","side":"maker"}\n'

ANALYSIS_RAW_JSONL = textwrap.dedent(
    """\
    {"ts_local":"2026-04-06T00:00:00.000000Z","side":"maker","exchange":"EDGEX","symbol":"BTC","exchange_ts":"2026-04-06T08:00:00+08:00","quote_lag_ms":0,"bids":[{"price":"100","qty":"1"}],"asks":[{"price":"100.5","qty":"1"}]}
    {"ts_local":"2026-04-06T00:00:01.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:01+08:00","quote_lag_ms":0,"bids":[{"price":"101.0","qty":"1"}],"asks":[{"price":"101.2","qty":"1"}]}
    {"ts_local":"2026-04-06T00:00:02.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:02+08:00","quote_lag_ms":0,"bids":[{"price":"101.1","qty":"1"}],"asks":[{"price":"101.3","qty":"1"}]}
    {"ts_local":"2026-04-06T00:00:03.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:03+08:00","quote_lag_ms":0,"bids":[{"price":"100.4","qty":"1"}],"asks":[{"price":"100.6","qty":"1"}]}
    {"ts_local":"2026-04-06T00:00:04.000000Z","side":"maker","exchange":"EDGEX","symbol":"BTC","exchange_ts":"2026-04-06T08:00:04+08:00","quote_lag_ms":0,"bids":[{"price":"101.2","qty":"1"}],"asks":[{"price":"101.6","qty":"1"}]}
    {"ts_local":"2026-04-06T00:00:05.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:05+08:00","quote_lag_ms":0,"bids":[{"price":"100.8","qty":"1"}],"asks":[{"price":"100.9","qty":"1"}]}
    {"ts_local":"2026-04-06T00:00:06.000000Z","side":"taker","exchange":"LIGHTER","symbol":"BTC","exchange_ts":"2026-04-06T08:00:06+08:00","quote_lag_ms":0,"bids":[{"price":"101.4","qty":"1"}],"asks":[{"price":"101.7","qty":"1"}]}
    """
)


class SpreadStoreTest(unittest.TestCase):
    def test_import_run_persists_run_and_points(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            db_path = Path(tmpdir) / "spread.db"
            raw_path = Path(tmpdir) / "logs" / "20260406_094223_EDGEX_LIGHTER_ETH" / "raw.jsonl"
            raw_path.parent.mkdir(parents=True)
            raw_path.write_text(SAMPLE_RAW_JSONL, encoding="utf-8")

            store = SpreadStore(db_path)
            result = store.import_run(raw_path, source_type="logs")
            runs = store.list_runs()
            points = store.get_run_points(result["id"])

        self.assertEqual(result["import_status"], "ready")
        self.assertFalse(result["skipped"])
        self.assertEqual(len(runs), 1)
        self.assertEqual(runs[0]["point_count"], 3)
        self.assertEqual(len(points), 3)
        self.assertAlmostEqual(points[0]["spread_ab_bps"], 98.52216748768473)

    def test_import_run_skips_unchanged_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            db_path = Path(tmpdir) / "spread.db"
            raw_path = Path(tmpdir) / "raw.jsonl"
            raw_path.write_text(SAMPLE_RAW_JSONL, encoding="utf-8")

            store = SpreadStore(db_path)
            first = store.import_run(raw_path, source_type="logs")
            second = store.import_run(raw_path, source_type="logs")

        self.assertFalse(first["skipped"])
        self.assertTrue(second["skipped"])
        self.assertEqual(second["id"], first["id"])

    def test_scan_logs_records_failed_imports_without_stopping(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            logs_dir = Path(tmpdir) / "logs"
            good_raw = logs_dir / "20260406_094223_EDGEX_LIGHTER_ETH" / "raw.jsonl"
            bad_raw = logs_dir / "20260406_094224_EDGEX_LIGHTER_BTC" / "raw.jsonl"
            good_raw.parent.mkdir(parents=True)
            bad_raw.parent.mkdir(parents=True)
            good_raw.write_text(SAMPLE_RAW_JSONL, encoding="utf-8")
            bad_raw.write_text(BAD_RAW_JSONL, encoding="utf-8")

            store = SpreadStore(Path(tmpdir) / "spread.db")
            results = scan_logs(store, logs_dir)
            runs = store.list_runs(include_failed=True)

        self.assertEqual(len(results), 2)
        self.assertEqual(len(runs), 2)
        self.assertEqual(sum(1 for run in runs if run["import_status"] == "ready"), 1)
        self.assertEqual(sum(1 for run in runs if run["import_status"] == "failed"), 1)


class SpreadServerTest(unittest.TestCase):
    def test_runs_and_points_endpoints_return_json(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            db_path = Path(tmpdir) / "spread.db"
            logs_dir = Path(tmpdir) / "logs"
            raw_path = logs_dir / "20260406_094223_EDGEX_LIGHTER_ETH" / "raw.jsonl"
            raw_path.parent.mkdir(parents=True)
            raw_path.write_text(SAMPLE_RAW_JSONL, encoding="utf-8")

            app = create_app(db_path=db_path, logs_dir=logs_dir, upload_dir=Path(tmpdir) / "uploads")
            runs_response = app.handle_request("GET", "/api/runs")
            runs = json.loads(runs_response["body"])
            run_id = runs[0]["id"]
            points_response = app.handle_request("GET", f"/api/runs/{run_id}/points")
            points = json.loads(points_response["body"])

        self.assertEqual(runs_response["status"], 200)
        self.assertEqual(len(runs), 1)
        self.assertEqual(points_response["status"], 200)
        self.assertEqual(len(points), 3)

    def test_upload_endpoint_imports_new_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            app = create_app(
                db_path=Path(tmpdir) / "spread.db",
                logs_dir=Path(tmpdir) / "logs",
                upload_dir=Path(tmpdir) / "uploads",
            )

            response = app.handle_upload("uploaded.jsonl", SAMPLE_RAW_JSONL.encode("utf-8"))
            runs = json.loads(app.handle_request("GET", "/api/runs")["body"])

        self.assertEqual(response["status"], 201)
        self.assertEqual(runs[0]["source_type"], "upload")
        self.assertEqual(runs[0]["file_name"], "uploaded.jsonl")

    def test_analysis_endpoint_recomputes_threshold_metrics_and_intervals(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            db_path = Path(tmpdir) / "spread.db"
            logs_dir = Path(tmpdir) / "logs"
            raw_path = logs_dir / "20260406_094223_EDGEX_LIGHTER_BTC" / "raw.jsonl"
            raw_path.parent.mkdir(parents=True)
            raw_path.write_text(ANALYSIS_RAW_JSONL, encoding="utf-8")

            app = create_app(db_path=db_path, logs_dir=logs_dir, upload_dir=Path(tmpdir) / "uploads")
            run_id = json.loads(app.handle_request("GET", "/api/runs")["body"])[0]["id"]
            lo_response = app.handle_request(
                "GET",
                f"/api/runs/{run_id}/analysis?threshold_bps=5&bucket_size_bps=1",
            )
            hi_response = app.handle_request(
                "GET",
                f"/api/runs/{run_id}/analysis?threshold_bps=50&bucket_size_bps=1",
            )

        low = json.loads(lo_response["body"])
        high = json.loads(hi_response["body"])

        self.assertEqual(lo_response["status"], 200)
        self.assertEqual(hi_response["status"], 200)
        self.assertEqual(low["ab"]["opportunity_count"], 1)
        self.assertEqual(low["ab"]["total_opportunity_duration_ms"], 1000)
        self.assertEqual(low["ab"]["longest_opportunity_duration_ms"], 1000)
        self.assertEqual(len(low["ab"]["intervals"]), 1)
        self.assertEqual(low["ab"]["intervals"][0]["duration_ms"], 1000)
        self.assertGreater(low["ab"]["intervals"][0]["max_spread_bps"], 40)
        self.assertLess(high["ab"]["above_threshold_samples"], low["ab"]["above_threshold_samples"])

    def test_analysis_endpoint_rebuckets_histogram(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            db_path = Path(tmpdir) / "spread.db"
            logs_dir = Path(tmpdir) / "logs"
            raw_path = logs_dir / "20260406_094223_EDGEX_LIGHTER_BTC" / "raw.jsonl"
            raw_path.parent.mkdir(parents=True)
            raw_path.write_text(ANALYSIS_RAW_JSONL, encoding="utf-8")

            app = create_app(db_path=db_path, logs_dir=logs_dir, upload_dir=Path(tmpdir) / "uploads")
            run_id = json.loads(app.handle_request("GET", "/api/runs")["body"])[0]["id"]
            fine_response = app.handle_request(
                "GET",
                f"/api/runs/{run_id}/analysis?threshold_bps=5&bucket_size_bps=1",
            )
            coarse_response = app.handle_request(
                "GET",
                f"/api/runs/{run_id}/analysis?threshold_bps=5&bucket_size_bps=20",
            )

        fine = json.loads(fine_response["body"])
        coarse = json.loads(coarse_response["body"])

        self.assertEqual(fine_response["status"], 200)
        self.assertEqual(coarse_response["status"], 200)
        self.assertGreater(len(fine["ab"]["histogram"]), len(coarse["ab"]["histogram"]))
        self.assertGreater(len(fine["ba"]["histogram"]), len(coarse["ba"]["histogram"]))


if __name__ == "__main__":
    unittest.main()
