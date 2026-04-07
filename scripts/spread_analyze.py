#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import math
import sys
from collections import Counter
from dataclasses import dataclass
from datetime import datetime
from decimal import Decimal
from pathlib import Path
from typing import Iterable, Iterator, Sequence, TextIO


@dataclass(frozen=True)
class TopOfBook:
    side: str
    exchange: str
    symbol: str
    ts_local: datetime
    exchange_ts: datetime | None
    bid_price: Decimal
    bid_qty: Decimal
    ask_price: Decimal
    ask_qty: Decimal


@dataclass(frozen=True)
class ReplayPoint:
    ts_local: datetime
    symbol: str
    maker_exchange: str
    taker_exchange: str
    maker_bid: Decimal
    maker_ask: Decimal
    taker_bid: Decimal
    taker_ask: Decimal
    spread_ab_bps: float
    spread_ba_bps: float


@dataclass(frozen=True)
class SpreadSummary:
    points: int
    ab_max: ReplayPoint
    ab_min: ReplayPoint
    ba_max: ReplayPoint
    ba_min: ReplayPoint


@dataclass(frozen=True)
class ProgressUpdate:
    line_number: int
    bytes_read: int
    total_bytes: int
    current_point: ReplayPoint | None
    summary: SpreadSummary | None
    analysis: "AnalysisSummary | None"
    should_print_progress: bool


@dataclass(frozen=True)
class HistogramBucket:
    lower_bps: float
    upper_bps: float
    count: int


@dataclass(frozen=True)
class OpportunityInterval:
    start_ts_local: datetime
    end_ts_local: datetime
    duration_ms: int
    max_spread_bps: float


@dataclass(frozen=True)
class DirectionAnalysis:
    samples: int
    min_spread_bps: float
    max_spread_bps: float
    mean_spread_bps: float
    stddev_spread_bps: float
    above_threshold_samples: int
    opportunity_count: int
    total_opportunity_duration_ms: int
    longest_opportunity_duration_ms: int
    best_opportunity_spread_bps: float
    histogram: tuple[HistogramBucket, ...]
    intervals: tuple[OpportunityInterval, ...]


@dataclass(frozen=True)
class AnalysisSummary:
    threshold_bps: float
    bucket_size_bps: float
    ab: DirectionAnalysis
    ba: DirectionAnalysis


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Replay raw orderbook logs and print spread extrema."
    )
    parser.add_argument(
        "path",
        nargs="?",
        default="logs",
        help="Path to logs root, a single run directory, or raw.jsonl (default: logs)",
    )
    parser.add_argument(
        "--progress-every",
        type=int,
        default=10000,
        help="Print progress every N input lines (default: 10000, 0 to disable)",
    )
    parser.add_argument(
        "--threshold",
        type=float,
        default=5.0,
        help="Opportunity threshold in bps for both AB and BA directions (default: 5.0)",
    )
    parser.add_argument(
        "--bucket-size",
        type=float,
        default=1.0,
        help="Histogram bucket size in bps for both directions (default: 1.0)",
    )
    return parser.parse_args(argv)


def parse_timestamp(value: str | None) -> datetime | None:
    if not value:
        return None
    normalized = value.replace("Z", "+00:00")
    return datetime.fromisoformat(normalized)


def parse_decimal(value: str | int | float) -> Decimal:
    return Decimal(str(value))


def first_level(levels: list[dict]) -> tuple[Decimal, Decimal]:
    if not levels:
        raise ValueError("missing top-of-book levels")
    best = levels[0]
    return parse_decimal(best["price"]), parse_decimal(best["qty"])


def parse_record(line: str) -> TopOfBook:
    payload = json.loads(line)
    bid_price, bid_qty = first_level(payload["bids"])
    ask_price, ask_qty = first_level(payload["asks"])
    return TopOfBook(
        side=payload["side"],
        exchange=payload["exchange"],
        symbol=payload["symbol"],
        ts_local=parse_timestamp(payload["ts_local"]),
        exchange_ts=parse_timestamp(payload.get("exchange_ts")),
        bid_price=bid_price,
        bid_qty=bid_qty,
        ask_price=ask_price,
        ask_qty=ask_qty,
    )


def spread_bps(long_price: Decimal, short_price: Decimal, mid_price: Decimal) -> float:
    return float((short_price - long_price) / mid_price * Decimal("10000"))


def resolve_raw_path(path: str | Path) -> Path:
    candidate = Path(path)
    if candidate.is_file():
        if candidate.name != "raw.jsonl":
            raise FileNotFoundError(f"expected raw.jsonl, got: {candidate}")
        return candidate

    direct_raw = candidate / "raw.jsonl"
    if direct_raw.is_file():
        return direct_raw

    raw_files = sorted(candidate.glob("*/raw.jsonl"))
    if raw_files:
        return raw_files[-1]

    raise FileNotFoundError(f"no raw.jsonl found under {candidate}")


def build_replay_point(record: TopOfBook, maker: TopOfBook, taker: TopOfBook) -> ReplayPoint:
    maker_mid = (maker.bid_price + maker.ask_price) / Decimal("2")
    taker_mid = (taker.bid_price + taker.ask_price) / Decimal("2")
    mid_price = (maker_mid + taker_mid) / Decimal("2")
    if mid_price == 0:
        raise ValueError("mid price is zero")

    return ReplayPoint(
        ts_local=record.ts_local,
        symbol=record.symbol,
        maker_exchange=maker.exchange,
        taker_exchange=taker.exchange,
        maker_bid=maker.bid_price,
        maker_ask=maker.ask_price,
        taker_bid=taker.bid_price,
        taker_ask=taker.ask_price,
        spread_ab_bps=spread_bps(maker.ask_price, taker.bid_price, mid_price),
        spread_ba_bps=spread_bps(taker.ask_price, maker.bid_price, mid_price),
    )


def iter_replay_spreads(raw_path: str | Path) -> Iterator[ReplayPoint]:
    path = Path(raw_path)
    maker: TopOfBook | None = None
    taker: TopOfBook | None = None

    with path.open("r", encoding="utf-8") as handle:
        for line in handle:
            if not line.strip():
                continue
            record = parse_record(line)
            if record.side == "maker":
                maker = record
            elif record.side == "taker":
                taker = record
            else:
                continue

            if maker is None or taker is None:
                continue

            try:
                yield build_replay_point(record, maker, taker)
            except ValueError:
                continue


def replay_spreads(raw_path: str | Path) -> list[ReplayPoint]:
    return list(iter_replay_spreads(raw_path))


def summarize_spreads(points: Iterable[ReplayPoint]) -> SpreadSummary:
    iterator = iter(points)
    try:
        first = next(iterator)
    except StopIteration as exc:
        raise ValueError("no replay points generated from raw logs") from exc

    count = 1
    ab_max = first
    ab_min = first
    ba_max = first
    ba_min = first

    for point in iterator:
        count += 1
        if point.spread_ab_bps > ab_max.spread_ab_bps:
            ab_max = point
        if point.spread_ab_bps < ab_min.spread_ab_bps:
            ab_min = point
        if point.spread_ba_bps > ba_max.spread_ba_bps:
            ba_max = point
        if point.spread_ba_bps < ba_min.spread_ba_bps:
            ba_min = point

    return SpreadSummary(
        points=count,
        ab_max=ab_max,
        ab_min=ab_min,
        ba_max=ba_max,
        ba_min=ba_min,
    )


def summarize_raw_path(raw_path: str | Path) -> SpreadSummary:
    return summarize_spreads(iter_replay_spreads(raw_path))


def summarize_latest(path: str | Path) -> tuple[Path, SpreadSummary]:
    raw_path = resolve_raw_path(path)
    return raw_path, summarize_raw_path(raw_path)


def update_summary(summary: SpreadSummary | None, point: ReplayPoint) -> SpreadSummary:
    if summary is None:
        return SpreadSummary(
            points=1,
            ab_max=point,
            ab_min=point,
            ba_max=point,
            ba_min=point,
        )

    return SpreadSummary(
        points=summary.points + 1,
        ab_max=point if point.spread_ab_bps > summary.ab_max.spread_ab_bps else summary.ab_max,
        ab_min=point if point.spread_ab_bps < summary.ab_min.spread_ab_bps else summary.ab_min,
        ba_max=point if point.spread_ba_bps > summary.ba_max.spread_ba_bps else summary.ba_max,
        ba_min=point if point.spread_ba_bps < summary.ba_min.spread_ba_bps else summary.ba_min,
    )


def new_direction_state(bucket_size_bps: float, threshold_bps: float) -> dict:
    return {
        "bucket_size_bps": bucket_size_bps,
        "threshold_bps": threshold_bps,
        "samples": 0,
        "mean": 0.0,
        "m2": 0.0,
        "min": math.inf,
        "max": -math.inf,
        "above_threshold_samples": 0,
        "opportunity_count": 0,
        "total_opportunity_duration_ms": 0,
        "longest_opportunity_duration_ms": 0,
        "best_opportunity_spread_bps": -math.inf,
        "active_start_ts": None,
        "active_last_ts": None,
        "active_max_spread_bps": -math.inf,
        "histogram": Counter(),
        "intervals": [],
    }


def duration_ms(start: datetime, end: datetime) -> int:
    return int((end - start).total_seconds() * 1000)


def clone_direction_state(state: dict) -> dict:
    cloned = dict(state)
    cloned["histogram"] = Counter(state["histogram"])
    cloned["intervals"] = list(state["intervals"])
    return cloned


def close_active_opportunity(state: dict) -> None:
    start = state["active_start_ts"]
    end = state["active_last_ts"]
    if start is None or end is None:
        return

    duration = duration_ms(start, end)
    state["intervals"].append(
        OpportunityInterval(
            start_ts_local=start,
            end_ts_local=end,
            duration_ms=duration,
            max_spread_bps=state["active_max_spread_bps"],
        )
    )
    state["opportunity_count"] += 1
    state["total_opportunity_duration_ms"] += duration
    if duration > state["longest_opportunity_duration_ms"]:
        state["longest_opportunity_duration_ms"] = duration
    if state["active_max_spread_bps"] > state["best_opportunity_spread_bps"]:
        state["best_opportunity_spread_bps"] = state["active_max_spread_bps"]
    state["active_start_ts"] = None
    state["active_last_ts"] = None
    state["active_max_spread_bps"] = -math.inf


def update_direction_state(state: dict, ts: datetime, spread_bps_value: float) -> None:
    state["samples"] += 1
    delta = spread_bps_value - state["mean"]
    state["mean"] += delta / state["samples"]
    delta2 = spread_bps_value - state["mean"]
    state["m2"] += delta * delta2
    state["min"] = min(state["min"], spread_bps_value)
    state["max"] = max(state["max"], spread_bps_value)

    bucket_size = state["bucket_size_bps"]
    bucket_index = math.floor(spread_bps_value / bucket_size)
    state["histogram"][bucket_index] += 1

    threshold = state["threshold_bps"]
    if spread_bps_value > threshold:
        state["above_threshold_samples"] += 1
        if state["active_start_ts"] is None:
            state["active_start_ts"] = ts
            state["active_last_ts"] = ts
            state["active_max_spread_bps"] = spread_bps_value
        else:
            state["active_last_ts"] = ts
            state["active_max_spread_bps"] = max(state["active_max_spread_bps"], spread_bps_value)
        return

    close_active_opportunity(state)


def finalize_direction_state(state: dict) -> DirectionAnalysis:
    close_active_opportunity(state)

    samples = state["samples"]
    if samples == 0:
        raise ValueError("no replay points generated from raw logs")

    variance = 0.0 if samples == 1 else state["m2"] / samples
    bucket_size = state["bucket_size_bps"]
    histogram = tuple(
        HistogramBucket(
            lower_bps=index * bucket_size,
            upper_bps=(index + 1) * bucket_size,
            count=count,
        )
        for index, count in sorted(state["histogram"].items())
    )
    best_opportunity_spread_bps = state["best_opportunity_spread_bps"]
    if best_opportunity_spread_bps == -math.inf:
        best_opportunity_spread_bps = float("nan")

    return DirectionAnalysis(
        samples=samples,
        min_spread_bps=state["min"],
        max_spread_bps=state["max"],
        mean_spread_bps=state["mean"],
        stddev_spread_bps=math.sqrt(variance),
        above_threshold_samples=state["above_threshold_samples"],
        opportunity_count=state["opportunity_count"],
        total_opportunity_duration_ms=state["total_opportunity_duration_ms"],
        longest_opportunity_duration_ms=state["longest_opportunity_duration_ms"],
        best_opportunity_spread_bps=best_opportunity_spread_bps,
        histogram=histogram,
        intervals=tuple(state["intervals"]),
    )


def analyze_points(
    points: Iterable[ReplayPoint],
    threshold_bps: float,
    bucket_size_bps: float,
) -> AnalysisSummary:
    ab_state = new_direction_state(bucket_size_bps, threshold_bps)
    ba_state = new_direction_state(bucket_size_bps, threshold_bps)

    for point in points:
        update_direction_state(ab_state, point.ts_local, point.spread_ab_bps)
        update_direction_state(ba_state, point.ts_local, point.spread_ba_bps)

    return AnalysisSummary(
        threshold_bps=threshold_bps,
        bucket_size_bps=bucket_size_bps,
        ab=finalize_direction_state(ab_state),
        ba=finalize_direction_state(ba_state),
    )


def summarize_raw_path_with_progress(
    raw_path: str | Path,
    progress_every: int = 0,
    threshold_bps: float = 5.0,
    bucket_size_bps: float = 1.0,
) -> Iterator[ProgressUpdate]:
    path = Path(raw_path)
    total_bytes = path.stat().st_size
    summary: SpreadSummary | None = None
    analysis: AnalysisSummary | None = None
    bytes_read = 0
    maker: TopOfBook | None = None
    taker: TopOfBook | None = None
    last_line_number = 0
    last_progress_line = 0
    ab_state = new_direction_state(bucket_size_bps, threshold_bps)
    ba_state = new_direction_state(bucket_size_bps, threshold_bps)

    with path.open("r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, start=1):
            last_line_number = line_number
            bytes_read += len(line.encode("utf-8"))
            if not line.strip():
                if progress_every > 0 and line_number % progress_every == 0:
                    last_progress_line = line_number
                    yield ProgressUpdate(
                        line_number=line_number,
                        bytes_read=bytes_read,
                        total_bytes=total_bytes,
                        current_point=None,
                        summary=summary,
                        analysis=analysis,
                        should_print_progress=True,
                    )
                continue

            record = parse_record(line)
            current_point: ReplayPoint | None = None

            if record.side == "maker":
                maker = record
            elif record.side == "taker":
                taker = record

            if maker is not None and taker is not None:
                try:
                    current_point = build_replay_point(record, maker, taker)
                except ValueError:
                    current_point = None
                else:
                    summary = update_summary(summary, current_point)
                    update_direction_state(ab_state, current_point.ts_local, current_point.spread_ab_bps)
                    update_direction_state(ba_state, current_point.ts_local, current_point.spread_ba_bps)
                    analysis = AnalysisSummary(
                        threshold_bps=threshold_bps,
                        bucket_size_bps=bucket_size_bps,
                        ab=finalize_direction_state(clone_direction_state(ab_state)),
                        ba=finalize_direction_state(clone_direction_state(ba_state)),
                    )

            if progress_every > 0 and line_number % progress_every == 0:
                last_progress_line = line_number
                yield ProgressUpdate(
                    line_number=line_number,
                    bytes_read=bytes_read,
                    total_bytes=total_bytes,
                    current_point=current_point,
                    summary=summary,
                    analysis=analysis,
                    should_print_progress=True,
                )

    if summary is None:
        raise ValueError("no replay points generated from raw logs")

    analysis = AnalysisSummary(
        threshold_bps=threshold_bps,
        bucket_size_bps=bucket_size_bps,
        ab=finalize_direction_state(ab_state),
        ba=finalize_direction_state(ba_state),
    )

    if progress_every > 0 and last_line_number > last_progress_line:
        yield ProgressUpdate(
            line_number=last_line_number,
            bytes_read=bytes_read,
            total_bytes=total_bytes,
            current_point=None,
            summary=summary,
            analysis=analysis,
            should_print_progress=True,
        )

    yield ProgressUpdate(
        line_number=last_line_number,
        bytes_read=bytes_read,
        total_bytes=total_bytes,
        current_point=None,
        summary=summary,
        analysis=analysis,
        should_print_progress=False,
    )


def format_progress(update: ProgressUpdate) -> str:
    percent = 100.0 if update.total_bytes == 0 else update.bytes_read * 100 / update.total_bytes
    current = "current=no-spread"
    if update.current_point is not None:
        current = (
            f"current_ab={update.current_point.spread_ab_bps:.2f}bps "
            f"current_ba={update.current_point.spread_ba_bps:.2f}bps "
            f"ts={update.current_point.ts_local.isoformat()}"
        )
    extrema = ""
    if update.summary is not None:
        extrema = (
            f" | points={update.summary.points}"
            f" ab_max={update.summary.ab_max.spread_ab_bps:.2f}bps"
            f" ab_min={update.summary.ab_min.spread_ab_bps:.2f}bps"
        )
    return (
        f"progress: line={update.line_number} bytes={update.bytes_read}/{update.total_bytes} "
        f"({percent:.2f}%) {current}{extrema}"
    )


def format_duration(duration_ms_value: int) -> str:
    if duration_ms_value < 1000:
        return f"{duration_ms_value}ms"
    seconds = duration_ms_value / 1000
    if seconds < 60:
        return f"{seconds:.3f}s"
    minutes, seconds = divmod(seconds, 60)
    return f"{int(minutes)}m{seconds:06.3f}s"


def format_histogram(direction: str, histogram: tuple[HistogramBucket, ...]) -> list[str]:
    lines = [f"{direction} histogram:"]
    for bucket in histogram:
        lines.append(
            f"  [{bucket.lower_bps:.2f}, {bucket.upper_bps:.2f}) bps: {bucket.count}"
        )
    return lines


def format_direction_analysis(direction: str, analysis: DirectionAnalysis, threshold_bps: float) -> list[str]:
    avg_duration_ms = (
        0
        if analysis.opportunity_count == 0
        else analysis.total_opportunity_duration_ms // analysis.opportunity_count
    )
    best_spread = "n/a"
    if not math.isnan(analysis.best_opportunity_spread_bps):
        best_spread = f"{analysis.best_opportunity_spread_bps:.2f}bps"

    return [
        f"{direction} opportunities > {threshold_bps:.2f} bps",
        f"  samples: {analysis.samples}",
        f"  spread min/max: {analysis.min_spread_bps:.2f}bps / {analysis.max_spread_bps:.2f}bps",
        f"  spread mean/std: {analysis.mean_spread_bps:.2f}bps / {analysis.stddev_spread_bps:.2f}bps",
        f"  above-threshold samples: {analysis.above_threshold_samples}",
        f"  occurrences: {analysis.opportunity_count}",
        f"  total duration: {format_duration(analysis.total_opportunity_duration_ms)}",
        f"  avg duration: {format_duration(avg_duration_ms)}",
        f"  longest duration: {format_duration(analysis.longest_opportunity_duration_ms)}",
        f"  best opportunity spread: {best_spread}",
    ]


def format_point(label: str, point: ReplayPoint, spread_bps_value: float) -> str:
    return (
        f"{label}: {spread_bps_value:.2f} bps | ts={point.ts_local.isoformat()} "
        f"| {point.maker_exchange}/{point.taker_exchange} "
        f"| maker bid/ask={point.maker_bid}/{point.maker_ask} "
        f"| taker bid/ask={point.taker_bid}/{point.taker_ask}"
    )


def main(argv: Sequence[str] | None = None, stdout: TextIO | None = None) -> int:
    args = parse_args(argv)
    output = stdout if stdout is not None else sys.stdout
    raw_path = resolve_raw_path(args.path)
    final_update: ProgressUpdate | None = None

    for update in summarize_raw_path_with_progress(
        raw_path,
        progress_every=args.progress_every,
        threshold_bps=args.threshold,
        bucket_size_bps=args.bucket_size,
    ):
        final_update = update
        if args.progress_every > 0 and update.should_print_progress and update.line_number > 0:
            print(format_progress(update), file=output)

    if final_update is None or final_update.summary is None or final_update.analysis is None:
        raise ValueError("no replay points generated from raw logs")

    summary = final_update.summary
    analysis = final_update.analysis
    print(f"source: {raw_path}", file=output)
    print(f"replay points: {summary.points}", file=output)
    print("AB (long maker, short taker)", file=output)
    print(format_point("  max", summary.ab_max, summary.ab_max.spread_ab_bps), file=output)
    print(format_point("  min", summary.ab_min, summary.ab_min.spread_ab_bps), file=output)
    print("BA (long taker, short maker)", file=output)
    print(format_point("  max", summary.ba_max, summary.ba_max.spread_ba_bps), file=output)
    print(format_point("  min", summary.ba_min, summary.ba_min.spread_ba_bps), file=output)
    print("Distribution", file=output)
    for line in format_direction_analysis("AB", analysis.ab, analysis.threshold_bps):
        print(line, file=output)
    for line in format_histogram("AB", analysis.ab.histogram):
        print(line, file=output)
    for line in format_direction_analysis("BA", analysis.ba, analysis.threshold_bps):
        print(line, file=output)
    for line in format_histogram("BA", analysis.ba.histogram):
        print(line, file=output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
