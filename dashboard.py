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
from urllib.parse import parse_qs, urlsplit
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
REPORT_NAME = "ai-work-report.html"
# The skill's report is self-contained HTML with inline scripts and Google Fonts. Serve it sandboxed
# (opaque origin) so its scripts can't call this dashboard's API.
REPORT_CSP = ("sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; img-src data:; "
              "style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com")
KEY_COLS = 4  # hour, tool, profile, model


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


class HistoryStore:
    """Hourly rows kept on disk so usage survives tools deleting old transcripts.

    Rows are keyed by (hour, tool, profile, model) and one key can combine several transcripts.
    A past hour's usage only grows, so a live row replaces the stored one unless it has fewer
    requests (some of its transcripts were deleted); stored rows the live scan no longer sees at
    all are kept as they are. Delete the file to rebuild it from the current transcripts.
    """

    def __init__(self, path):
        self.path = path
        self.rows = {}
        self.sessions = {}
        if path and os.path.exists(path):
            try:
                with open(path, encoding="utf-8") as fh:
                    saved = json.load(fh)
                if saved.get("columns") == COLUMNS:
                    self.rows = {tuple(r[:KEY_COLS]): r for r in saved.get("rows", [])}
                    self.sessions = {f"{x['tool']}|{x['profile']}|{x['id']}": x for x in saved.get("sessions", [])}
                else:
                    print(f"history: column layout changed; starting fresh ({path})", file=sys.stderr)
            except (OSError, ValueError) as exc:
                print(f"history: could not read {path}: {exc}; starting fresh", file=sys.stderr)

    def merge(self, live_rows, live_sessions=()):
        """Merge a live scan; returns the keys whose stored row outlived (part of) its transcripts."""
        changed, live_keys, held = False, set(), set()
        for x in live_sessions:
            key = f"{x['tool']}|{x['profile']}|{x['id']}"
            old = self.sessions.get(key)
            if old is None or (x["requests"] >= old["requests"] and x != old):
                self.sessions[key] = x
                changed = True
        req = COLUMNS.index("requests")
        for r in live_rows:
            key = tuple(r[:KEY_COLS])
            live_keys.add(key)
            old = self.rows.get(key)
            if old is not None and r[req] < old[req]:
                held.add(key)
                continue
            if old != r:
                self.rows[key] = r
                changed = True
        if changed:
            self.save()
        return held | (set(self.rows) - live_keys)

    def save(self):
        if not self.path:
            return
        os.makedirs(os.path.dirname(self.path), exist_ok=True)
        tmp = f"{self.path}.tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump({"columns": COLUMNS, "rows": sorted(self.rows.values()),
                       "sessions": sorted(self.sessions.values(), key=lambda x: x["last"])}, fh, separators=(",", ":"))
        os.replace(tmp, self.path)

    def since(self, first_hour):
        return [r for r in self.rows.values() if r[0] >= first_hour]

    def detail(self, key):
        x = self.sessions.get(key)
        return x.get("detail") if x else None

    def sessions_since(self, cutoff_iso):
        return sorted((x for x in self.sessions.values() if x["last"] >= cutoff_iso), key=lambda x: x["last"], reverse=True)


def load_work_report(folder):
    """Read the ai-work-report skill's classified.json / narrative.json from its workdir, if present."""
    if not folder or not os.path.isdir(folder):
        return None
    out, problems = {"dir": folder}, []
    for name in ("classified", "narrative"):
        path = os.path.join(folder, f"{name}.json")
        if not os.path.exists(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                out[name] = json.load(fh)
            out[f"{name}_mtime"] = dt.datetime.fromtimestamp(os.path.getmtime(path)).astimezone().isoformat(timespec="seconds")
        except (OSError, ValueError) as exc:
            problems.append(f"{name}.json: {exc}")
    report = os.path.join(folder, REPORT_NAME)
    if os.path.exists(report):
        out["report_mtime"] = dt.datetime.fromtimestamp(os.path.getmtime(report)).astimezone().isoformat(timespec="seconds")
    out["error"] = "; ".join(problems) or None
    if "classified" not in out and "report_mtime" not in out:
        return {"dir": folder, "error": out["error"]} if problems else None
    c = out.get("classified")
    if c is not None and not (isinstance(c, dict) and isinstance(c.get("themes"), list) and isinstance(c.get("sessions"), dict)):
        out.pop("classified")
        out["error"] = "classified.json: expected {themes: [...], sessions: {...}}"
    return out


class Collector:
    """Builds snapshots on a timer, or right away when asked."""

    def __init__(self, args):
        self.args = args
        self.collect = load_collect()
        self.cache = self.collect.FileCache()
        self.history = HistoryStore(args.history)
        self.lock = threading.Lock()
        self.wake = threading.Event()
        self.body, self.etag = b'{"pending":true}', '"pending"'
        self.last_error = None

    def build(self):
        c, a = self.collect, self.args
        started = time.monotonic()
        now = dt.datetime.now().astimezone()
        cutoff = (now - dt.timedelta(days=a.days - 1)).replace(hour=0, minute=0, second=0, microsecond=0)
        usage, status = c.Usage(cutoff, bucket="hour"), []
        self.cache.start()
        c.collect_claude(usage, c.default_claude_dirs(), status, self.cache)
        c.collect_codex(usage, a.codex_home, status, self.cache)
        c.collect_copilot(usage, a.copilot_db, status)
        c.collect_proxy(usage, a.proxy_log, status)
        self.cache.prune()
        budgets, budget_error = load_budgets(a.config)
        live = [[k[3], k[0], k[1], k[2], *(r[m] for m in c.METRICS), len(r["sessions"]), round(r["copilot_aiu"], 4)]
                for k, r in usage.rows.items()]
        first_hour = cutoff.strftime("%Y-%m-%dT%H:00")
        sessions_live = c.session_rows(usage, detail=True)
        held = self.history.merge(live, sessions_live)
        rows = sorted(self.history.since(first_hour))
        kept = sum(1 for k in held if k[0] >= first_hour)
        # Session details (prompts, files, commands) stay server-side; the page fetches one on demand.
        sessions = [{k: v for k, v in x.items() if k != "detail"}
                    for x in self.history.sessions_since(cutoff.isoformat(timespec="seconds"))]
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
                "cache_hits": self.cache.hits,
                "cache_misses": self.cache.misses,
                "history": c.tilde(a.history) if a.history else None,
                "history_rows": len(self.history.rows),
                "history_only_rows": max(kept, 0),
                "history_since": min(self.history.rows)[0] if self.history.rows else None,
                "work_report_dir": c.tilde(a.work_report) if a.work_report else None,
                "config_error": budget_error,
            },
            "sources": [dict(zip(("source", "path", "status", "requests"), s)) for s in status],
            "budgets": budgets,
            "columns": COLUMNS,
            "rows": rows,
            "sessions": sessions,
            "work_report": load_work_report(a.work_report),
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
        print(f"collected {len(snap['rows'])} rows in {m['collect_ms']} ms "
              f"(files re-read {m['cache_misses']}, cached {m['cache_hits']}, history-only rows {m['history_only_rows']})",
              file=sys.stderr)

    def run(self):
        # main() sets wake, so the first pass collects immediately; then every tick or wake-up.
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
            elif path == "/api/session":
                key = (parse_qs(urlsplit(self.path).query).get("key") or [""])[0]
                with collector.lock:
                    detail = collector.history.detail(key)
                if detail is None:
                    self.send(404, b'{"error":"unknown session"}')
                else:
                    self.send(200, json.dumps(detail, separators=(",", ":")).encode())
            elif path == "/report":
                report = os.path.join(collector.args.work_report or "", REPORT_NAME)
                try:
                    with open(report, "rb") as fh:
                        self.send(200, fh.read(), "text/html; charset=utf-8",
                                  extra={"Content-Security-Policy": REPORT_CSP})
                except OSError:
                    self.send(404, b'{"error":"no ai-work-report.html in the work report folder yet"}')
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
    ap.add_argument("--history", default=os.path.join(HERE, "data", "hourly.json"),
                    help="hourly history store, kept after tools delete transcripts (default ./data/hourly.json; "
                         "'' to disable)")
    ap.add_argument("--work-report", default=os.path.join(HERE, "data", "work-report"),
                    help="ai-work-report skill workdir to overlay (classified.json, narrative.json, "
                         f"{REPORT_NAME}; default ./data/work-report)")
    ap.add_argument("--codex-home", default=os.environ.get("CODEX_HOME", "~/.codex"))
    ap.add_argument("--copilot-db", default="~/.copilot/session-store.db")
    ap.add_argument("--proxy-log", default="~/Library/Logs/foundry-entra-proxy/proxy.log")
    args = ap.parse_args()
    args.interval = max(args.interval, 5)
    args.days = min(max(args.days, 2), 400)

    collector = Collector(args)
    collector.wake.set()  # first collection runs in the thread so the server answers right away
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
