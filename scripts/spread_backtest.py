#!/usr/bin/env python3

from __future__ import annotations

import argparse
import bisect
import math
import sys
from collections import deque
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Callable, Iterable, Sequence, TextIO

if __package__ is None or __package__ == "":
    sys.path.append(str(Path(__file__).resolve().parents[1]))

from scripts.spread_analyze import ReplayPoint, iter_replay_spreads, resolve_raw_path


@dataclass(frozen=True)
class BacktestConfig:
    window: int
    open_buffer_bps: float
    close_buffer_bps: float
    stop_buffer_bps: float
    fee_bps: float
    max_hold_seconds: int


@dataclass(frozen=True)
class Trade:
    direction: str
    entry_ts: datetime
    exit_ts: datetime
    entry_spread_bps: float
    exit_spread_bps: float
    gross_pnl_bps: float
    net_pnl_bps: float
    hold_ms: int
    exit_reason: str


@dataclass(frozen=True)
class DirectionBacktestSummary:
    trade_count: int
    win_count: int
    total_net_pnl_bps: float
    average_net_pnl_bps: float
    average_hold_ms: int


@dataclass(frozen=True)
class BacktestResult:
    points: int
    total_trades: int
    winning_trades: int
    total_net_pnl_bps: float
    average_net_pnl_bps: float
    total_hold_ms: int
    average_hold_ms: int
    best_trade_net_pnl_bps: float
    worst_trade_net_pnl_bps: float
    ab: DirectionBacktestSummary
    ba: DirectionBacktestSummary
    trades: list[Trade]


@dataclass
class OpenPosition:
    direction: str
    entry_ts: datetime
    entry_spread_bps: float


class RollingMedian:
    def __init__(self, window: int) -> None:
        if window <= 0:
            raise ValueError("window must be > 0")
        self.window = window
        self.queue: deque[float] = deque()
        self.sorted_values: list[float] = []

    def ready(self) -> bool:
        return len(self.queue) >= self.window

    def median(self) -> float:
        if not self.ready():
            raise ValueError("rolling median is not ready")
        size = len(self.sorted_values)
        middle = size // 2
        if size % 2 == 1:
            return self.sorted_values[middle]
        return (self.sorted_values[middle - 1] + self.sorted_values[middle]) / 2

    def add(self, value: float) -> None:
        bisect.insort(self.sorted_values, value)
        self.queue.append(value)
        if len(self.queue) > self.window:
            removed = self.queue.popleft()
            index = bisect.bisect_left(self.sorted_values, removed)
            del self.sorted_values[index]


def direction_spread(point: ReplayPoint, direction: str) -> float:
    if direction == "AB":
        return point.spread_ab_bps
    if direction == "BA":
        return point.spread_ba_bps
    raise ValueError(f"unknown direction: {direction}")


def opposite_spread(point: ReplayPoint, direction: str) -> float:
    if direction == "AB":
        return point.spread_ba_bps
    if direction == "BA":
        return point.spread_ab_bps
    raise ValueError(f"unknown direction: {direction}")


def hold_ms(entry_ts: datetime, exit_ts: datetime) -> int:
    return int((exit_ts - entry_ts).total_seconds() * 1000)


def summarize_direction(trades: Iterable[Trade], direction: str) -> DirectionBacktestSummary:
    matched = [trade for trade in trades if trade.direction == direction]
    if not matched:
        return DirectionBacktestSummary(
            trade_count=0,
            win_count=0,
            total_net_pnl_bps=0.0,
            average_net_pnl_bps=0.0,
            average_hold_ms=0,
        )

    total_net = sum(trade.net_pnl_bps for trade in matched)
    total_hold = sum(trade.hold_ms for trade in matched)
    win_count = sum(1 for trade in matched if trade.net_pnl_bps > 0)
    return DirectionBacktestSummary(
        trade_count=len(matched),
        win_count=win_count,
        total_net_pnl_bps=total_net,
        average_net_pnl_bps=total_net / len(matched),
        average_hold_ms=total_hold // len(matched),
    )


def close_trade(
    trades: list[Trade],
    position: OpenPosition,
    point: ReplayPoint,
    config: BacktestConfig,
    exit_reason: str,
) -> None:
    exit_spread = opposite_spread(point, position.direction)
    gross_pnl = position.entry_spread_bps + exit_spread
    net_pnl = gross_pnl - config.fee_bps
    trades.append(
        Trade(
            direction=position.direction,
            entry_ts=position.entry_ts,
            exit_ts=point.ts_local,
            entry_spread_bps=position.entry_spread_bps,
            exit_spread_bps=exit_spread,
            gross_pnl_bps=gross_pnl,
            net_pnl_bps=net_pnl,
            hold_ms=hold_ms(position.entry_ts, point.ts_local),
            exit_reason=exit_reason,
        )
    )


def choose_entry_direction(
    point: ReplayPoint,
    ab_baseline: float,
    ba_baseline: float,
    config: BacktestConfig,
) -> str | None:
    candidates: list[tuple[float, float, str]] = []
    ab_threshold = ab_baseline + config.open_buffer_bps
    if point.spread_ab_bps > ab_threshold:
        candidates.append((point.spread_ab_bps - ab_threshold, point.spread_ab_bps, "AB"))

    ba_threshold = ba_baseline + config.open_buffer_bps
    if point.spread_ba_bps > ba_threshold:
        candidates.append((point.spread_ba_bps - ba_threshold, point.spread_ba_bps, "BA"))

    if not candidates:
        return None

    candidates.sort(reverse=True)
    return candidates[0][2]


def backtest_points(points: Iterable[ReplayPoint], config: BacktestConfig) -> BacktestResult:
    return run_backtest(points, config)


def run_backtest(
    points: Iterable[ReplayPoint],
    config: BacktestConfig,
    progress_every: int = 0,
    on_progress: Callable[[int, list[Trade], OpenPosition | None], None] | None = None,
) -> BacktestResult:
    ab_window = RollingMedian(config.window)
    ba_window = RollingMedian(config.window)
    trades: list[Trade] = []
    position: OpenPosition | None = None
    points_seen = 0
    last_point: ReplayPoint | None = None

    for point in points:
        points_seen += 1
        last_point = point

        ab_baseline = ab_window.median() if ab_window.ready() else None
        ba_baseline = ba_window.median() if ba_window.ready() else None

        if position is not None:
            current_spread = direction_spread(point, position.direction)
            baseline = ab_baseline if position.direction == "AB" else ba_baseline
            reason = ""
            if baseline is not None and current_spread <= baseline + config.stop_buffer_bps:
                reason = "stop"
            elif baseline is not None and current_spread <= baseline + config.close_buffer_bps:
                reason = "reversion"
            elif hold_ms(position.entry_ts, point.ts_local) >= config.max_hold_seconds * 1000:
                reason = "max_hold"

            if reason:
                close_trade(trades, position, point, config, reason)
                position = None
        elif ab_baseline is not None and ba_baseline is not None:
            direction = choose_entry_direction(point, ab_baseline, ba_baseline, config)
            if direction is not None:
                position = OpenPosition(
                    direction=direction,
                    entry_ts=point.ts_local,
                    entry_spread_bps=direction_spread(point, direction),
                )

        ab_window.add(point.spread_ab_bps)
        ba_window.add(point.spread_ba_bps)

        if progress_every > 0 and on_progress is not None and points_seen % progress_every == 0:
            on_progress(points_seen, trades, position)

    if position is not None and last_point is not None:
        close_trade(trades, position, last_point, config, "end_of_data")

    total_trades = len(trades)
    if total_trades == 0:
        return BacktestResult(
            points=points_seen,
            total_trades=0,
            winning_trades=0,
            total_net_pnl_bps=0.0,
            average_net_pnl_bps=0.0,
            total_hold_ms=0,
            average_hold_ms=0,
            best_trade_net_pnl_bps=0.0,
            worst_trade_net_pnl_bps=0.0,
            ab=summarize_direction(trades, "AB"),
            ba=summarize_direction(trades, "BA"),
            trades=trades,
        )

    total_net = sum(trade.net_pnl_bps for trade in trades)
    total_hold = sum(trade.hold_ms for trade in trades)
    winning_trades = sum(1 for trade in trades if trade.net_pnl_bps > 0)
    return BacktestResult(
        points=points_seen,
        total_trades=total_trades,
        winning_trades=winning_trades,
        total_net_pnl_bps=total_net,
        average_net_pnl_bps=total_net / total_trades,
        total_hold_ms=total_hold,
        average_hold_ms=total_hold // total_trades,
        best_trade_net_pnl_bps=max(trade.net_pnl_bps for trade in trades),
        worst_trade_net_pnl_bps=min(trade.net_pnl_bps for trade in trades),
        ab=summarize_direction(trades, "AB"),
        ba=summarize_direction(trades, "BA"),
        trades=trades,
    )


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Backtest a median-buffer spread strategy on raw.jsonl replay data."
    )
    parser.add_argument(
        "path",
        nargs="?",
        default="logs",
        help="Path to logs root, a single run directory, or raw.jsonl (default: logs)",
    )
    parser.add_argument("--window", type=int, default=300, help="Rolling median window in replay points")
    parser.add_argument("--open-buffer", type=float, default=5.0, help="Open when spread > median + open-buffer")
    parser.add_argument("--close-buffer", type=float, default=1.0, help="Close when spread <= median + close-buffer")
    parser.add_argument("--stop-buffer", type=float, default=-3.0, help="Stop when spread <= median + stop-buffer")
    parser.add_argument("--fee-bps", type=float, default=0.0, help="Round-trip fee drag in bps")
    parser.add_argument("--max-hold-seconds", type=int, default=300, help="Force close after this many seconds")
    parser.add_argument("--progress-every", type=int, default=100000, help="Print progress every N replay points, 0 to disable")
    parser.add_argument("--show-trades", type=int, default=5, help="Show up to N recent trades in the final summary")
    return parser.parse_args(argv)


def format_duration(duration_ms_value: int) -> str:
    if duration_ms_value < 1000:
        return f"{duration_ms_value}ms"
    seconds = duration_ms_value / 1000
    if seconds < 60:
        return f"{seconds:.3f}s"
    minutes, seconds = divmod(seconds, 60)
    return f"{int(minutes)}m{seconds:06.3f}s"


def format_win_rate(win_count: int, trade_count: int) -> str:
    if trade_count == 0:
        return "0.00%"
    return f"{win_count * 100 / trade_count:.2f}%"


def format_trade(trade: Trade) -> str:
    return (
        f"{trade.direction} entry={trade.entry_ts.isoformat()} exit={trade.exit_ts.isoformat()} "
        f"entry_spread={trade.entry_spread_bps:.2f}bps exit_spread={trade.exit_spread_bps:.2f}bps "
        f"gross={trade.gross_pnl_bps:.2f}bps net={trade.net_pnl_bps:.2f}bps "
        f"hold={format_duration(trade.hold_ms)} reason={trade.exit_reason}"
    )


def print_direction_summary(name: str, summary: DirectionBacktestSummary, output: TextIO) -> None:
    print(f"{name} summary", file=output)
    print(f"  trades: {summary.trade_count}", file=output)
    print(f"  wins: {summary.win_count} ({format_win_rate(summary.win_count, summary.trade_count)})", file=output)
    print(f"  total net pnl: {summary.total_net_pnl_bps:.2f}bps", file=output)
    print(f"  avg net pnl: {summary.average_net_pnl_bps:.2f}bps", file=output)
    print(f"  avg hold: {format_duration(summary.average_hold_ms)}", file=output)


def main(argv: Sequence[str] | None = None, stdout: TextIO | None = None) -> int:
    args = parse_args(argv)
    output = stdout if stdout is not None else sys.stdout
    raw_path = resolve_raw_path(args.path)
    config = BacktestConfig(
        window=args.window,
        open_buffer_bps=args.open_buffer,
        close_buffer_bps=args.close_buffer,
        stop_buffer_bps=args.stop_buffer,
        fee_bps=args.fee_bps,
        max_hold_seconds=args.max_hold_seconds,
    )

    def print_progress(points_seen: int, trades: list[Trade], position: OpenPosition | None) -> None:
        current_net = sum(trade.net_pnl_bps for trade in trades)
        print(
            f"progress: points={points_seen} trades={len(trades)} net={current_net:.2f}bps "
            f"open_position={position.direction if position is not None else 'none'}",
            file=output,
        )

    result = run_backtest(
        iter_replay_spreads(raw_path),
        config,
        progress_every=args.progress_every,
        on_progress=print_progress,
    )

    print(f"source: {raw_path}", file=output)
    print(
        "config: "
        f"window={config.window} open={config.open_buffer_bps:.2f}bps "
        f"close={config.close_buffer_bps:.2f}bps stop={config.stop_buffer_bps:.2f}bps "
        f"fee={config.fee_bps:.2f}bps max_hold={config.max_hold_seconds}s",
        file=output,
    )
    print(f"replay points: {result.points}", file=output)
    print(f"trades: {result.total_trades}", file=output)
    print(f"wins: {result.winning_trades} ({format_win_rate(result.winning_trades, result.total_trades)})", file=output)
    print(f"total net pnl: {result.total_net_pnl_bps:.2f}bps", file=output)
    print(f"avg net pnl: {result.average_net_pnl_bps:.2f}bps", file=output)
    print(f"best / worst trade: {result.best_trade_net_pnl_bps:.2f}bps / {result.worst_trade_net_pnl_bps:.2f}bps", file=output)
    print(f"avg hold: {format_duration(result.average_hold_ms)}", file=output)
    print_direction_summary("AB", result.ab, output)
    print_direction_summary("BA", result.ba, output)

    if args.show_trades > 0 and result.trades:
        print(f"recent trades (last {min(args.show_trades, len(result.trades))})", file=output)
        for trade in result.trades[-args.show_trades :]:
            print(f"  {format_trade(trade)}", file=output)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
