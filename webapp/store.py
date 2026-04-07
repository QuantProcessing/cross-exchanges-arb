from __future__ import annotations

import sqlite3
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterable

from scripts.spread_analyze import AnalysisSummary, analyze_points, iter_replay_spreads


@dataclass(frozen=True)
class StoredPoint:
    ts_local: datetime
    symbol: str
    maker_exchange: str
    taker_exchange: str
    spread_ab_bps: float
    spread_ba_bps: float


def utc_now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def fingerprint_for(path: Path) -> tuple[int, float, str]:
    stat = path.stat()
    return stat.st_size, stat.st_mtime, f"{stat.st_size}:{stat.st_mtime_ns}"


def serialize_direction(direction) -> dict[str, object]:
    payload = asdict(direction)
    payload["histogram"] = [
        {
            "lower_bps": bucket["lower_bps"],
            "upper_bps": bucket["upper_bps"],
            "count": bucket["count"],
        }
        for bucket in payload["histogram"]
    ]
    payload["intervals"] = [
        {
            "start_ts_local": interval["start_ts_local"].isoformat(),
            "end_ts_local": interval["end_ts_local"].isoformat(),
            "duration_ms": interval["duration_ms"],
            "max_spread_bps": interval["max_spread_bps"],
        }
        for interval in payload["intervals"]
    ]
    return payload


def serialize_analysis(summary: AnalysisSummary) -> dict[str, object]:
    return {
        "threshold_bps": summary.threshold_bps,
        "bucket_size_bps": summary.bucket_size_bps,
        "ab": serialize_direction(summary.ab),
        "ba": serialize_direction(summary.ba),
    }


class SpreadStore:
    def __init__(self, db_path: str | Path) -> None:
        self.db_path = Path(db_path)
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._ensure_schema()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA foreign_keys = ON")
        return conn

    def _ensure_schema(self) -> None:
        with self._connect() as conn:
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS runs (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    run_name TEXT NOT NULL,
                    file_name TEXT NOT NULL,
                    source_type TEXT NOT NULL,
                    source_path TEXT NOT NULL UNIQUE,
                    symbol TEXT,
                    maker_exchange TEXT,
                    taker_exchange TEXT,
                    point_count INTEGER NOT NULL DEFAULT 0,
                    file_size_bytes INTEGER NOT NULL,
                    file_mtime REAL NOT NULL,
                    file_fingerprint TEXT NOT NULL,
                    import_status TEXT NOT NULL,
                    error_message TEXT,
                    imported_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS spread_points (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    run_id INTEGER NOT NULL,
                    ts_local TEXT NOT NULL,
                    symbol TEXT NOT NULL,
                    maker_exchange TEXT NOT NULL,
                    taker_exchange TEXT NOT NULL,
                    spread_ab_bps REAL NOT NULL,
                    spread_ba_bps REAL NOT NULL,
                    FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE
                );

                CREATE INDEX IF NOT EXISTS idx_spread_points_run_ts
                ON spread_points(run_id, ts_local);
                """
            )

    def import_run(self, raw_path: str | Path, source_type: str) -> dict[str, object]:
        path = Path(raw_path)
        size, mtime, fingerprint = fingerprint_for(path)

        with self._connect() as conn:
            existing = conn.execute(
                "SELECT * FROM runs WHERE source_path = ?",
                (str(path),),
            ).fetchone()
            if existing is not None and existing["file_fingerprint"] == fingerprint:
                payload = dict(existing)
                payload["skipped"] = True
                return payload

        try:
            points = [
                StoredPoint(
                    ts_local=point.ts_local,
                    symbol=point.symbol,
                    maker_exchange=point.maker_exchange,
                    taker_exchange=point.taker_exchange,
                    spread_ab_bps=point.spread_ab_bps,
                    spread_ba_bps=point.spread_ba_bps,
                )
                for point in iter_replay_spreads(path)
            ]
            if not points:
                raise ValueError("no replay points generated from raw logs")
        except Exception as exc:  # noqa: BLE001
            return self._upsert_run(
                raw_path=path,
                source_type=source_type,
                size=size,
                mtime=mtime,
                fingerprint=fingerprint,
                import_status="failed",
                error_message=str(exc),
                points=[],
            )

        return self._upsert_run(
            raw_path=path,
            source_type=source_type,
            size=size,
            mtime=mtime,
            fingerprint=fingerprint,
            import_status="ready",
            error_message=None,
            points=points,
        )

    def _upsert_run(
        self,
        raw_path: Path,
        source_type: str,
        size: int,
        mtime: float,
        fingerprint: str,
        import_status: str,
        error_message: str | None,
        points: Iterable[StoredPoint],
    ) -> dict[str, object]:
        points = list(points)
        first = points[0] if points else None
        run_name = raw_path.parent.name if raw_path.parent.name else raw_path.stem
        imported_at = utc_now_iso()

        with self._connect() as conn:
            existing = conn.execute(
                "SELECT id FROM runs WHERE source_path = ?",
                (str(raw_path),),
            ).fetchone()

            if existing is None:
                cursor = conn.execute(
                    """
                    INSERT INTO runs (
                        run_name, file_name, source_type, source_path, symbol,
                        maker_exchange, taker_exchange, point_count, file_size_bytes,
                        file_mtime, file_fingerprint, import_status, error_message, imported_at
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        run_name,
                        raw_path.name,
                        source_type,
                        str(raw_path),
                        first.symbol if first else None,
                        first.maker_exchange if first else None,
                        first.taker_exchange if first else None,
                        len(points),
                        size,
                        mtime,
                        fingerprint,
                        import_status,
                        error_message,
                        imported_at,
                    ),
                )
                run_id = cursor.lastrowid
            else:
                run_id = existing["id"]
                conn.execute("DELETE FROM spread_points WHERE run_id = ?", (run_id,))
                conn.execute(
                    """
                    UPDATE runs
                    SET run_name = ?, file_name = ?, source_type = ?, symbol = ?,
                        maker_exchange = ?, taker_exchange = ?, point_count = ?,
                        file_size_bytes = ?, file_mtime = ?, file_fingerprint = ?,
                        import_status = ?, error_message = ?, imported_at = ?
                    WHERE id = ?
                    """,
                    (
                        run_name,
                        raw_path.name,
                        source_type,
                        first.symbol if first else None,
                        first.maker_exchange if first else None,
                        first.taker_exchange if first else None,
                        len(points),
                        size,
                        mtime,
                        fingerprint,
                        import_status,
                        error_message,
                        imported_at,
                        run_id,
                    ),
                )

            if points:
                conn.executemany(
                    """
                    INSERT INTO spread_points (
                        run_id, ts_local, symbol, maker_exchange, taker_exchange,
                        spread_ab_bps, spread_ba_bps
                    ) VALUES (?, ?, ?, ?, ?, ?, ?)
                    """,
                    [
                        (
                            run_id,
                            point.ts_local.isoformat(),
                            point.symbol,
                            point.maker_exchange,
                            point.taker_exchange,
                            point.spread_ab_bps,
                            point.spread_ba_bps,
                        )
                        for point in points
                    ],
                )

            conn.commit()

        payload = self.get_run(run_id)
        payload["skipped"] = False
        return payload

    def list_runs(self, include_failed: bool = False) -> list[dict[str, object]]:
        query = "SELECT * FROM runs"
        params: tuple[object, ...] = ()
        if not include_failed:
            query += " WHERE import_status = ?"
            params = ("ready",)
        query += " ORDER BY imported_at DESC, id DESC"
        with self._connect() as conn:
            rows = conn.execute(query, params).fetchall()
        return [dict(row) for row in rows]

    def get_run(self, run_id: int) -> dict[str, object]:
        with self._connect() as conn:
            row = conn.execute("SELECT * FROM runs WHERE id = ?", (run_id,)).fetchone()
        if row is None:
            raise KeyError(f"unknown run id: {run_id}")
        return dict(row)

    def get_run_points(self, run_id: int) -> list[dict[str, object]]:
        with self._connect() as conn:
            rows = conn.execute(
                """
                SELECT ts_local, symbol, maker_exchange, taker_exchange, spread_ab_bps, spread_ba_bps
                FROM spread_points
                WHERE run_id = ?
                ORDER BY ts_local
                """,
                (run_id,),
            ).fetchall()
        return [dict(row) for row in rows]

    def get_run_analysis(
        self,
        run_id: int,
        threshold_bps: float,
        bucket_size_bps: float,
    ) -> dict[str, object]:
        rows = self.get_run_points(run_id)
        points = [
            StoredPoint(
                ts_local=datetime.fromisoformat(row["ts_local"]),
                symbol=row["symbol"],
                maker_exchange=row["maker_exchange"],
                taker_exchange=row["taker_exchange"],
                spread_ab_bps=row["spread_ab_bps"],
                spread_ba_bps=row["spread_ba_bps"],
            )
            for row in rows
        ]
        return serialize_analysis(
            analyze_points(
                points,
                threshold_bps=threshold_bps,
                bucket_size_bps=bucket_size_bps,
            )
        )


def scan_logs(store: SpreadStore, logs_dir: str | Path) -> list[dict[str, object]]:
    base = Path(logs_dir)
    if not base.exists():
        return []
    return [store.import_run(raw_path, source_type="logs") for raw_path in sorted(base.glob("*/raw.jsonl"))]
