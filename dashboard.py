#!/usr/bin/env python3
"""Local, live AI token usage dashboard built on collect.py.

Re-runs the collect.py collectors in a background thread (hourly buckets) and
serves a single-page dashboard plus a JSON snapshot on a loopback address.
Only usage counters, model names, timestamps, and session counts leave the
collectors; prompt and response text is never read into the snapshot.

Run with:  python3 -I dashboard.py [--port 8790] [--interval 30] [--days 35]
Then open: http://127.0.0.1:8790
"""
import argparse
import datetime as dt
import hashlib
import importlib.util
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
STATIC = os.path.join(HERE, "static")
STATIC_FILES = {
    "/": ("index.html", "text/html; charset=utf-8"),
    "/app.js": ("app.js", "text/javascript; charset=utf-8"),
    "/app.css": ("app.css", "text/css; charset=utf-8"),
}
COLUMNS = ["hour", "tool", "profile", "model", "requests", "input", "cached", "cache_write",
           "output", "reasoning", "total", "sessions", "copilot_aiu"]
SECURITY_HEADERS = {
    "Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "
                               "connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "no-referrer",
    "Cache-Control": "no-cache",
}
PERIODS = ("day", "week", "month")


def load_collect():
    # python3 -I leaves the script directory off sys.path, so load collect.py by path.
    spec = importlib.util.spec_from_file_location("collect", os.path.join(HERE, "collect.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load_budgets(path):
    """Read optional budgets from dashboard.json. Returns (budgets, error)."""
    if not path or not os.path.exists(path):
        return {}, None
    try:
        with open(path, encoding="utf-8") as fh:
            raw = json.load(fh).get("budgets") or {}
    except (OSError, ValueError, AttributeError) as exc:
        return {}, f"{os.path.basename(path)}: {exc}"
    budgets, problems = {}, []
    for key, b in raw.items():
        if key.startswith("_"):
            continue
        if not isinstance(b, dict) or ":" not in key:
            problems.append(f"{key}: expected \"tool:profile\": {{...}}")
            continue
        period = b.get("period", "month")
        tokens = b.get("tokens")
        if period not in PERIODS or not isinstance(tokens, (int, float)) or tokens <= 0:
            problems.append(f"{key}: needs tokens > 0 and period in {PERIODS}")
            continue
        budgets[key] = {
            "tokens": int(tokens),
            "metric": b.get("metric", "total") if b.get("metric") in ("total", "uncached", "output") else "total",
            "period": period,
            "reset_day": min(max(int(b.get("reset_day", 1)), 1), 28),
            "reset_weekday": min(max(int(b.get("reset_weekday", 0)), 0), 6),
        }
    return budgets, "; ".join(problems) or None


class Collector:
    """Builds snapshots on a timer, or right away when asked."""

    def __init__(self, args):
        self.args = args
        self.collect = load_collect()
        self.lock = threading.Lock()
        self.wake = threading.Event()
        self.body, self.etag = b"{}", '"empty"'
        self.last_error = None

    def build(self):
        c, a = self.collect, self.args
        started = time.monotonic()
        now = dt.datetime.now().astimezone()
        cutoff = (now - dt.timedelta(days=a.days - 1)).replace(hour=0, minute=0, second=0, microsecond=0)
        usage, status = c.Usage(cutoff, bucket="hour"), []
        c.collect_claude(usage, list(c.CLAUDE_PROFILES.items()), status)
        c.collect_codex(usage, a.codex_home, status)
        c.collect_copilot(usage, a.copilot_db, status)
        c.collect_proxy(usage, a.proxy_log, status)
        budgets, budget_error = load_budgets(a.config)
        rows = [[k[3], k[0], k[1], k[2], *(r[m] for m in c.METRICS), len(r["sessions"]), round(r["copilot_aiu"], 4)]
                for k, r in sorted(usage.rows.items(), key=lambda kv: (kv[0][3], kv[0][:3]))]
        return {
            "meta": {
                "generated": now.isoformat(timespec="seconds"),
                "since": cutoff.isoformat(timespec="seconds"),
                "tz": str(now.tzinfo),
                "utc_offset_min": int(now.utcoffset().total_seconds() // 60),
                "days": a.days,
                "interval": a.interval,
                "collect_ms": int((time.monotonic() - started) * 1000),
                "config": c.tilde(a.config) if a.config else None,
                "config_exists": bool(a.config and os.path.exists(a.config)),
                "config_error": budget_error,
            },
            "sources": [dict(zip(("source", "path", "status", "requests"), s)) for s in status],
            "budgets": budgets,
            "columns": COLUMNS,
            "rows": rows,
        }

    def refresh(self):
        try:
            snap = self.build()
        except Exception as exc:  # keep serving the last good snapshot
            self.last_error = f"{type(exc).__name__}: {exc}"
            print(f"collect failed: {self.last_error}", file=sys.stderr)
            return
        body = json.dumps(snap, separators=(",", ":")).encode()
        etag = '"' + hashlib.sha256(body).hexdigest()[:16] + '"'
        with self.lock:
            self.body, self.etag, self.last_error = body, etag, None
        m = snap["meta"]
        print(f"collected {len(snap['rows'])} rows in {m['collect_ms']} ms", file=sys.stderr)

    def run(self):
        # main() already ran the first refresh; wait, then collect on each tick or wake-up.
        while True:
            self.wake.wait(self.args.interval)
            self.wake.clear()
            self.refresh()

    def snapshot(self):
        with self.lock:
            return self.body, self.etag


def make_handler(collector, allowed_hosts):
    class Handler(BaseHTTPRequestHandler):
        server_version = "ai-usage-dashboard"
        sys_version = ""

        def log_message(self, fmt, *args):
            if self.path.startswith("/api/usage") or self.path == "/healthz":
                return
            sys.stderr.write("%s %s\n" % (self.log_date_time_string(), fmt % args))

        def send(self, code, body=b"", ctype="application/json", extra=None):
            self.send_response(code)
            for k, v in {**SECURITY_HEADERS, **(extra or {})}.items():
                self.send_header(k, v)
            if body or code not in (204, 304):
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if body and self.command != "HEAD":
                self.wfile.write(body)

        def host_ok(self):
            # Reject DNS-rebinding requests whose Host isn't this loopback listener.
            if (self.headers.get("Host") or "").lower() in allowed_hosts:
                return True
            self.send(421, b'{"error":"unexpected Host header"}')
            return False

        def do_HEAD(self):
            self.do_GET()

        def do_GET(self):
            if not self.host_ok():
                return
            path = self.path.split("?", 1)[0]
            if path == "/healthz":
                self.send(200, b'{"status":"ok"}')
            elif path == "/api/usage":
                body, etag = collector.snapshot()
                if self.headers.get("If-None-Match") == etag:
                    self.send(304, extra={"ETag": etag})
                else:
                    self.send(200, body, extra={"ETag": etag})
            elif path in STATIC_FILES:
                name, ctype = STATIC_FILES[path]
                try:
                    with open(os.path.join(STATIC, name), "rb") as fh:
                        self.send(200, fh.read(), ctype)
                except OSError:
                    self.send(404, b'{"error":"missing static file"}')
            else:
                self.send(404, b'{"error":"not found"}')

        def do_POST(self):
            if not self.host_ok():
                return
            # A custom header can't be sent cross-origin without a CORS preflight this server never approves.
            if self.path != "/api/refresh" or self.headers.get("X-Requested-With") != "ai-usage":
                self.send(403, b'{"error":"forbidden"}')
                return
            collector.wake.set()
            self.send(202, b'{"status":"refreshing"}')

    return Handler


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1", choices=("127.0.0.1", "localhost"),
                    help="loopback address to bind (default 127.0.0.1)")
    ap.add_argument("--port", type=int, default=int(os.environ.get("AI_USAGE_PORT", 8790)))
    ap.add_argument("--interval", type=int, default=30, help="seconds between collections (default 30)")
    ap.add_argument("--days", type=int, default=35, help="history to load in days (default 35)")
    ap.add_argument("--config", default=os.path.join(HERE, "dashboard.json"),
                    help="budgets file (default ./dashboard.json; see dashboard.example.json)")
    ap.add_argument("--codex-home", default=os.environ.get("CODEX_HOME", "~/.codex"))
    ap.add_argument("--copilot-db", default="~/.copilot/session-store.db")
    ap.add_argument("--proxy-log", default="~/Library/Logs/foundry-entra-proxy/proxy.log")
    args = ap.parse_args()
    args.interval = max(args.interval, 5)
    args.days = min(max(args.days, 2), 400)

    collector = Collector(args)
    collector.refresh()
    threading.Thread(target=collector.run, daemon=True).start()

    names = {"127.0.0.1", "localhost"}
    allowed = {f"{n}:{args.port}" for n in names}
    server = ThreadingHTTPServer((args.host, args.port), make_handler(collector, allowed))
    server.daemon_threads = True
    print(f"ai-usage dashboard on http://{args.host}:{args.port} (refresh every {args.interval}s)", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
