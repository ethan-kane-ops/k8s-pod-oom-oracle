#!/usr/bin/env python3
"""Serve captured reports on the daemon's routes, for recording the demo.

The demo has to be reproducible, and a real daemon is not: it needs a cluster,
and the kills it records differ every run in timing, PIDs and trajectory shape,
so the GIF would change on every rebuild for reasons nobody chose.

The JSON beside this file is a real capture from a kind cluster, with the
workload names and the node name replaced. Nothing here fabricates a report; it
replays one. Standard library only, so `just demo` needs no environment.
"""

from __future__ import annotations

import argparse
import json
import pathlib
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

HERE = pathlib.Path(__file__).parent


class Handler(BaseHTTPRequestHandler):
    """The subset of the API the dashboard reads."""

    events: list = []
    status: dict = {}

    def do_GET(self) -> None:  # noqa: N802 - name fixed by BaseHTTPRequestHandler
        parsed = urlparse(self.path)
        route = parsed.path
        if route == "/v1/status":
            self._json(self.status)
        elif route == "/v1/events":
            self._json(self._filter(parse_qs(parsed.query)))
        elif route == "/healthz":
            self._text("ok")
        elif route == "/readyz":
            self._text("ready")
        else:
            self.send_error(404)

    def _filter(self, query: dict[str, list[str]]) -> list:
        """Apply the same filters store.Filter does.

        Ignoring them would make `inspect <pod>` print every report, which is
        not what the daemon does and would put the wrong thing in the demo.
        """
        selected = self.events
        for param, field in (
            ("namespace", "namespace"),
            ("pod", "podName"),
            ("container", "containerName"),
        ):
            want = query.get(param, [""])[0]
            if want:
                selected = [r for r in selected if r["identity"].get(field) == want]

        raw = query.get("limit", [""])[0]
        if raw:
            try:
                limit = int(raw)
            except ValueError:
                limit = 0
            if limit > 0:
                selected = selected[:limit]

        return selected

    def _json(self, payload) -> None:
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _text(self, text: str) -> None:
        body = text.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args) -> None:
        """Silence the access log; it would be recorded into the GIF."""


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=9090)
    args = parser.parse_args()

    Handler.events = json.loads((HERE / "events.json").read_text())
    Handler.status = json.loads((HERE / "status.json").read_text())

    HTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
