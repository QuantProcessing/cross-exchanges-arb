from __future__ import annotations

import argparse
import json
import mimetypes
import re
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from webapp.store import SpreadStore, scan_logs


RUN_RE = re.compile(r"^/api/runs/(\d+)$")
RUN_POINTS_RE = re.compile(r"^/api/runs/(\d+)/points$")
RUN_ANALYSIS_RE = re.compile(r"^/api/runs/(\d+)/analysis$")


@dataclass
class SpreadApp:
    store: SpreadStore
    logs_dir: Path
    upload_dir: Path
    static_dir: Path

    def __post_init__(self) -> None:
        self.upload_dir.mkdir(parents=True, exist_ok=True)
        scan_logs(self.store, self.logs_dir)

    def handle_request(self, method: str, path: str) -> dict[str, object]:
        parsed = urlparse(path)

        if method == "GET" and parsed.path == "/api/runs":
            return self._json(self.store.list_runs())

        if method == "GET":
            match = RUN_RE.match(parsed.path)
            if match:
                return self._lookup(lambda: self.store.get_run(int(match.group(1))))

            match = RUN_POINTS_RE.match(parsed.path)
            if match:
                return self._lookup(lambda: self.store.get_run_points(int(match.group(1))))

            match = RUN_ANALYSIS_RE.match(parsed.path)
            if match:
                params = parse_qs(parsed.query)
                try:
                    threshold_bps = float(params.get("threshold_bps", ["5"])[0])
                    bucket_size_bps = float(params.get("bucket_size_bps", ["1"])[0])
                except ValueError:
                    return self._json({"error": "invalid analysis parameters"}, status=HTTPStatus.BAD_REQUEST)
                if bucket_size_bps <= 0:
                    return self._json({"error": "bucket_size_bps must be > 0"}, status=HTTPStatus.BAD_REQUEST)
                return self._lookup(
                    lambda: self.store.get_run_analysis(
                        int(match.group(1)),
                        threshold_bps=threshold_bps,
                        bucket_size_bps=bucket_size_bps,
                    )
                )

            return self.handle_static_request(parsed.path)

        return self._json({"error": "method not allowed"}, status=HTTPStatus.METHOD_NOT_ALLOWED)

    def handle_upload(self, filename: str, body: bytes) -> dict[str, object]:
        safe_name = Path(filename).name or "upload.jsonl"
        if not safe_name.endswith(".jsonl"):
            return self._json({"error": "expected a .jsonl upload"}, status=HTTPStatus.BAD_REQUEST)
        target = self.upload_dir / safe_name
        target.write_bytes(body)
        result = self.store.import_run(target, source_type="upload")
        if result["import_status"] != "ready":
            return self._json({"error": result["error_message"]}, status=HTTPStatus.BAD_REQUEST)
        return self._json(result, status=HTTPStatus.CREATED)

    def handle_static_request(self, path: str) -> dict[str, object]:
        relative = path.strip("/") or "index.html"
        candidate = (self.static_dir / relative).resolve()
        if candidate == self.static_dir.resolve() or self.static_dir.resolve() in candidate.parents:
            if candidate.is_file():
                return self._static(candidate)
        return self._static(self.static_dir / "index.html")

    def _lookup(self, fn) -> dict[str, object]:
        try:
            return self._json(fn())
        except KeyError:
            return self._json({"error": "not found"}, status=HTTPStatus.NOT_FOUND)

    def _json(self, payload: object, status: HTTPStatus = HTTPStatus.OK) -> dict[str, object]:
        return {
            "status": int(status),
            "headers": {"Content-Type": "application/json; charset=utf-8"},
            "body": json.dumps(payload),
        }

    def _static(self, path: Path) -> dict[str, object]:
        content_type = mimetypes.guess_type(path.name)[0] or "text/html"
        body = path.read_text(encoding="utf-8")
        return {
            "status": int(HTTPStatus.OK),
            "headers": {"Content-Type": f"{content_type}; charset=utf-8"},
            "body": body,
        }


def create_app(db_path: str | Path, logs_dir: str | Path, upload_dir: str | Path) -> SpreadApp:
    return SpreadApp(
        store=SpreadStore(db_path),
        logs_dir=Path(logs_dir),
        upload_dir=Path(upload_dir),
        static_dir=Path(__file__).resolve().parent / "static",
    )


def run_server(host: str, port: int, db_path: str | Path, logs_dir: str | Path, upload_dir: str | Path) -> None:
    app = create_app(db_path=db_path, logs_dir=logs_dir, upload_dir=upload_dir)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802
            self._write(app.handle_request("GET", self.path))

        def do_POST(self) -> None:  # noqa: N802
            if self.path != "/api/uploads":
                self._write(app._json({"error": "not found"}, status=HTTPStatus.NOT_FOUND))
                return
            length = int(self.headers.get("Content-Length", "0"))
            filename = self.headers.get("X-Filename", "upload.jsonl")
            self._write(app.handle_upload(filename, self.rfile.read(length)))

        def _write(self, response: dict[str, object]) -> None:
            self.send_response(int(response["status"]))
            for key, value in response["headers"].items():
                self.send_header(key, value)
            self.end_headers()
            body = response["body"]
            if isinstance(body, str):
                body = body.encode("utf-8")
            self.wfile.write(body)

        def log_message(self, format: str, *args) -> None:  # noqa: A003
            return

    ThreadingHTTPServer((host, port), Handler).serve_forever()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Serve the spread analysis assistant site.")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--db-path", default="data/spread-analysis.db")
    parser.add_argument("--logs-dir", default="logs")
    parser.add_argument("--upload-dir", default="data/uploads")
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    run_server(
        host=args.host,
        port=args.port,
        db_path=args.db_path,
        logs_dir=args.logs_dir,
        upload_dir=args.upload_dir,
    )


if __name__ == "__main__":
    main()
