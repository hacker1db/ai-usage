#!/usr/bin/env python3
"""Collect local AI token usage and write a per tool/profile/model/day report.

Reads (read-only) Claude Code profile transcripts, Codex CLI session rollouts, the
GitHub Copilot CLI session store, and the foundry-entra-proxy request log. Only
usage counters, model names, timestamps, and session ids are read into the
report; prompt and response text is never copied.

Run with:  python3 -I collect.py [--days 30] [--out reports]
"""
import argparse
import csv
import datetime as dt
import glob
import json
import os
import sqlite3
import sys
from collections import defaultdict

HOME = os.path.expanduser("~")
METRICS = ("requests", "input", "cached", "cache_write", "output", "reasoning", "total")

# Claude Code profiles: CLAUDE_CONFIG_DIR -> profile label. ~/.claude is retired.
CLAUDE_PROFILES = {
    "~/.claude-personal": "personal",
    "~/.claude-subscription": "work-subscription",
    "~/.claude-foundry": "work-foundry",
}
# Codex model_provider id -> profile label.
CODEX_PROVIDERS = {"openai": "personal-chatgpt", "foundry": "work-foundry"}


def tilde(path):
    return "~" + path[len(HOME):] if path.startswith(HOME) else path


def parse_ts(value):
    """Parse ISO-8601 or epoch seconds into an aware datetime, or None."""
    if value is None or value == "":
        return None
    try:
        if isinstance(value, (int, float)):
            return dt.datetime.fromtimestamp(value, dt.timezone.utc)
        text = str(value).replace("Z", "+00:00")
        ts = dt.datetime.fromisoformat(text)
        return ts if ts.tzinfo else ts.replace(tzinfo=dt.timezone.utc)
    except (ValueError, OverflowError, OSError):
        return None


def iter_jsonl(path):
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            if not line.strip():
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(obj, dict):
                yield obj


class Usage:
    """Aggregates usage rows keyed by (tool, profile, model, local day or hour).

    bucket="day" keys by local date (YYYY-MM-DD); bucket="hour" keys by local hour
    (YYYY-MM-DDTHH:00), which the dashboard uses for burn rates.
    """

    def __init__(self, cutoff, bucket="day"):
        if bucket not in ("day", "hour"):
            raise ValueError(f"bucket must be day or hour, not {bucket!r}")
        self.cutoff = cutoff
        self.bucket = bucket
        self.rows = defaultdict(lambda: dict.fromkeys(METRICS, 0) | {"sessions": set(), "copilot_aiu": 0.0})

    def add(self, tool, profile, model, ts, session, **m):
        if ts is None or ts < self.cutoff:
            return False
        local = ts.astimezone()
        when = local.strftime("%Y-%m-%dT%H:00") if self.bucket == "hour" else local.date().isoformat()
        row = self.rows[(tool, profile, model or "unknown", when)]
        row["requests"] += 1
        for k in ("input", "cached", "cache_write", "output", "reasoning"):
            row[k] += int(m.get(k) or 0)
        row["total"] += int(m.get("input") or 0) + int(m.get("output") or 0)
        row["copilot_aiu"] += float(m.get("copilot_aiu") or 0)
        if session:
            row["sessions"].add(session)
        return True


class FileCache:
    """Parsed records per (parser, file), reused while the file's size and mtime are unchanged.

    The dashboard keeps one instance across refreshes so only new or growing transcripts are
    re-read; a one-shot collect.py run uses a throwaway instance.
    """

    def __init__(self):
        self.entries = {}
        self.hits = self.misses = 0
        self.live = set()

    def records(self, path, parse):
        try:
            st = os.stat(path)
        except OSError:
            return []
        key, sig = (parse.__name__, path), (st.st_size, st.st_mtime_ns)
        self.live.add(key)
        hit = self.entries.get(key)
        if hit and hit[0] == sig:
            self.hits += 1
            return hit[1]
        self.misses += 1
        recs = parse(path)
        self.entries[key] = (sig, recs)
        return recs

    def start(self):
        self.hits = self.misses = 0
        self.live = set()

    def prune(self):
        """Forget files that weren't seen in the last collection (deleted or out of window)."""
        for key in set(self.entries) - self.live:
            del self.entries[key]


def default_claude_dirs():
    """Current profiles first, then retired ~/.claude copies ("legacy"), then a live ~/.claude
    ("no-profile": sessions launched without CLAUDE_CONFIG_DIR). Dedupe keeps the first hit."""
    dirs = list(CLAUDE_PROFILES.items())
    for path in sorted(glob.glob(os.path.join(HOME, ".claude.retired-*"))):
        if os.path.isdir(os.path.join(path, "projects")):
            dirs.append((tilde(path), "legacy"))
    if os.path.isdir(os.path.join(HOME, ".claude", "projects")):
        dirs.append(("~/.claude", "no-profile"))
    return dirs


def parse_claude_file(path):
    """Usage records from one Claude Code transcript: (dedupe key, model, ts, session, metrics)."""
    out, fallback = [], os.path.basename(path)[:-6]
    for e in iter_jsonl(path):
        if e.get("type") != "assistant":
            continue
        msg = e.get("message") or {}
        u = msg.get("usage") or {}
        if not u or msg.get("model") == "<synthetic>":
            continue
        key = (msg.get("id"), e.get("requestId"))
        cr, cw = u.get("cache_read_input_tokens", 0) or 0, u.get("cache_creation_input_tokens", 0) or 0
        out.append((key if key != (None, None) else None, msg.get("model"), parse_ts(e.get("timestamp")),
                    e.get("sessionId") or fallback,
                    {"input": (u.get("input_tokens", 0) or 0) + cr + cw, "cached": cr, "cache_write": cw,
                     "output": u.get("output_tokens", 0)}))
    return out


def collect_claude(usage, dirs, status, cache=None):
    cache = cache or FileCache()
    # Streaming writes one line per content block with the same usage, and the same message can
    # exist in a retired copy of a profile dir; dedupe on (message.id, requestId) across all dirs.
    seen = set()
    for raw, label in dirs:
        root = os.path.realpath(os.path.expanduser(raw))
        name = f"claude-code:{label}"
        if not os.path.isdir(os.path.join(root, "projects")):
            status.append((name, tilde(root), "not found", 0))
            continue
        count = 0
        for path in glob.glob(os.path.join(root, "projects", "**", "*.jsonl"), recursive=True):
            if os.path.getmtime(path) < usage.cutoff.timestamp():
                continue
            for key, model, ts, session, m in cache.records(path, parse_claude_file):
                if key is not None:
                    if key in seen:
                        continue
                    seen.add(key)
                count += usage.add("claude-code", label, model, ts, session, **m)
        status.append((name, tilde(root), "ok", count))


def parse_codex_file(path):
    """Usage records from one Codex rollout: (response id or None, provider, model, ts, session, metrics)."""
    provider, model, session = None, None, os.path.basename(path)[:-6]
    events = list(iter_jsonl(path))
    has_records = any(e.get("type") == "token_usage_record" for e in events)
    prev_total, out = None, []
    for e in events:
        p = e.get("payload") if isinstance(e.get("payload"), dict) else {}
        kind = e.get("type")
        if kind == "session_meta":
            provider = p.get("model_provider") or provider
            session = p.get("id") or session
            model = p.get("model") or model
        elif kind == "turn_context":
            model = p.get("model") or model
            provider = p.get("model_provider_id") or p.get("model_provider") or provider
        elif p.get("type") == "thread_settings_applied":
            ts_ = p.get("thread_settings") or {}
            model = ts_.get("model") or model
            provider = ts_.get("model_provider_id") or provider
        u, rid = None, None
        if kind == "token_usage_record":
            rid = p.get("response_id")
            u = p.get("usage") or {}
        elif not has_records and p.get("type") == "token_count":
            # Older rollouts: diff the cumulative total so duplicate events don't double count.
            tot = ((p.get("info") or {}).get("total_token_usage")) or None
            if not tot:
                continue
            if prev_total and tot.get("total_tokens", 0) >= prev_total.get("total_tokens", 0):
                u = {k: (tot.get(k, 0) or 0) - (prev_total.get(k, 0) or 0) for k in tot}
            else:
                u = tot
            prev_total = tot
            if not any(u.values()):
                continue
        if u is None:
            continue
        out.append((rid, provider, model, parse_ts(e.get("timestamp")), session,
                    {"input": u.get("input_tokens"), "cached": u.get("cached_input_tokens"),
                     "cache_write": u.get("cache_write_input_tokens"), "output": u.get("output_tokens"),
                     "reasoning": u.get("reasoning_output_tokens")}))
    return out


def collect_codex(usage, root, status, cache=None):
    cache = cache or FileCache()
    root = os.path.expanduser(root)
    files = [p for d in ("sessions", "archived_sessions")
             for p in glob.glob(os.path.join(root, d, "**", "*.jsonl"), recursive=True)]
    if not files:
        status.append(("codex", tilde(root), "not found", 0))
        return
    count, seen = 0, set()
    for path in files:
        if os.path.getmtime(path) < usage.cutoff.timestamp():
            continue
        for rid, provider, model, ts, session, m in cache.records(path, parse_codex_file):
            if rid is not None:
                if rid in seen:
                    continue
                seen.add(rid)
            prof = CODEX_PROVIDERS.get(provider or "openai", provider)
            count += usage.add("codex", prof, model, ts, session, **m)
    status.append(("codex", tilde(root), "ok", count))


def collect_copilot(usage, db_path, status):
    db_path = os.path.expanduser(db_path)
    if not os.path.exists(db_path):
        status.append(("copilot", tilde(db_path), "not found", 0))
        return
    try:
        con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        rows = con.execute(
            "SELECT session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,"
            " reasoning_tokens, total_nano_aiu, created_at FROM assistant_usage_events WHERE created_at >= ?",
            (usage.cutoff.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S"),)).fetchall()
        con.close()
    except sqlite3.Error as exc:
        status.append(("copilot", tilde(db_path), f"error: {exc}", 0))
        return
    count = 0
    for sid, model, inp, out, cr, cw, rsn, aiu, created in rows:
        count += usage.add("copilot", "github-copilot", model, parse_ts(created), sid,
                           input=inp, cached=cr, cache_write=cw, output=out, reasoning=rsn,
                           copilot_aiu=(aiu or 0) / 1e9)
    status.append(("copilot", tilde(db_path), "ok", count))


def collect_proxy(usage, log_path, status):
    log_path = os.path.expanduser(log_path)
    if not os.path.exists(log_path):
        status.append(("foundry-proxy", tilde(log_path), "not found", 0))
        return
    count = 0
    for e in iter_jsonl(log_path):
        if e.get("msg") != "request":
            continue
        count += usage.add("foundry-proxy", e.get("client") or "unknown", e.get("model"),
                           parse_ts(e.get("time")), None,
                           input=e.get("input_tokens"), cached=e.get("cached_tokens"),
                           output=e.get("output_tokens"), reasoning=e.get("reasoning_tokens"))
    status.append(("foundry-proxy", tilde(log_path), "ok", count))


def rollup(rows, keyfn):
    out = defaultdict(lambda: dict.fromkeys(METRICS, 0) | {"sessions": set(), "copilot_aiu": 0.0})
    for key, row in rows.items():
        agg = out[keyfn(key)]
        for k in METRICS:
            agg[k] += row[k]
        agg["copilot_aiu"] += row["copilot_aiu"]
        agg["sessions"] |= {(key[0], key[1], s) for s in row["sessions"]}
    return out


def fmt(n):
    return f"{n:,}"


def table(title, groups, headers):
    lines = [f"## {title}", "", "| " + " | ".join(headers + ["Sessions", "Requests", "Input", "Cached",
                                                            "Output", "Reasoning", "Total"]) + " |",
             "|" + "---|" * (len(headers) + 7)]
    for key, r in sorted(groups.items(), key=lambda kv: (-kv[1]["total"]) if len(headers) < 3 else kv[0]):
        key = key if isinstance(key, tuple) else (key,)
        lines.append("| " + " | ".join(list(key) + [fmt(len(r["sessions"])), fmt(r["requests"]), fmt(r["input"]),
                                                     fmt(r["cached"]), fmt(r["output"]), fmt(r["reasoning"]),
                                                     fmt(r["total"])]) + " |")
    return lines + [""]


def render_markdown(meta, status, rows):
    main = {k: v for k, v in rows.items() if k[0] != "foundry-proxy"}
    proxy = {k: v for k, v in rows.items() if k[0] == "foundry-proxy"}
    grand = rollup(main, lambda k: "all")["all"] if main else None
    out = [f"# AI usage report ({meta['since']} to {meta['until']})", "",
           f"Generated {meta['generated']} by `collect.py`. Days are local time ({meta['tz']}).", "",
           "## Sources", "", "| Source | Path | Status | Requests in window |", "|---|---|---|---|"]
    out += [f"| {n} | `{p}` | {s} | {fmt(c)} |" for n, p, s, c in status] + [""]
    if grand:
        out += [f"**Total (excluding proxy cross-check):** {fmt(grand['total'])} tokens "
                f"({fmt(grand['input'])} input, of which {fmt(grand['cached'])} cached; {fmt(grand['output'])} output), "
                f"{fmt(grand['requests'])} requests, {fmt(len(grand['sessions']))} sessions.", ""]
    out += table("By tool and profile", rollup(main, lambda k: (k[0], k[1])), ["Tool", "Profile"])
    out += table("By model", rollup(main, lambda k: (k[0], k[1], k[2])), ["Tool", "Profile", "Model"])
    out += table("By day", rollup(main, lambda k: (k[3], k[0], k[1])), ["Day", "Tool", "Profile"])
    aiu = rollup(main, lambda k: k[2]) if any(k[0] == "copilot" for k in main) else {}
    cop = {m: r["copilot_aiu"] for m, r in aiu.items() if r["copilot_aiu"]}
    if cop:
        out += ["## Copilot AI units (local estimate, total_nano_aiu / 1e9)", "", "| Model | AIU |", "|---|---|"]
        out += [f"| {m} | {v:,.2f} |" for m, v in sorted(cop.items(), key=lambda kv: -kv[1])] + [""]
    if proxy:
        out += table("foundry-entra-proxy (cross-check; overlaps Codex work-foundry)",
                     rollup(proxy, lambda k: (k[1], k[2])), ["Client", "Model"])
    out += ["## Notes", "",
            "- Input includes cached and cache-write tokens for every tool, so Total = Input + Output.",
            "- Reasoning tokens are a subset of Output (OpenAI and Copilot); Claude logs do not report them.",
            "- Copilot premium-request billing is only authoritative in GitHub billing; see README.", ""]
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--days", type=int, default=30, help="window in days (default 30)")
    ap.add_argument("--out", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "reports"),
                    help="output root; a dated subfolder is created (default ./reports)")
    ap.add_argument("--claude", action="append", metavar="DIR=LABEL",
                    help="Claude config dir and profile label (repeatable; replaces defaults)")
    ap.add_argument("--codex-home", default=os.environ.get("CODEX_HOME", "~/.codex"))
    ap.add_argument("--copilot-db", default="~/.copilot/session-store.db")
    ap.add_argument("--proxy-log", default="~/Library/Logs/foundry-entra-proxy/proxy.log")
    ap.add_argument("--print", action="store_true", help="also print the markdown report")
    args = ap.parse_args()

    now = dt.datetime.now().astimezone()
    cutoff = (now - dt.timedelta(days=args.days - 1)).replace(hour=0, minute=0, second=0, microsecond=0)
    claude = [tuple(c.split("=", 1)) if "=" in c else (c, os.path.basename(c).lstrip(".")) for c in args.claude] \
        if args.claude else default_claude_dirs()

    usage, status = Usage(cutoff), []
    collect_claude(usage, claude, status)
    collect_codex(usage, args.codex_home, status)
    collect_copilot(usage, args.copilot_db, status)
    collect_proxy(usage, args.proxy_log, status)

    meta = {"generated": now.isoformat(timespec="seconds"), "since": cutoff.date().isoformat(),
            "until": now.date().isoformat(), "tz": str(now.tzinfo), "days": args.days}
    outdir = os.path.join(os.path.expanduser(args.out), now.date().isoformat())
    os.makedirs(outdir, exist_ok=True)
    flat = [{"tool": k[0], "profile": k[1], "model": k[2], "day": k[3], "sessions": len(r["sessions"]),
             **{m: r[m] for m in METRICS}, "copilot_aiu": round(r["copilot_aiu"], 4)}
            for k, r in sorted(usage.rows.items(), key=lambda kv: (kv[0][3], kv[0][:3]))]
    with open(os.path.join(outdir, "usage.json"), "w", encoding="utf-8") as fh:
        json.dump({"meta": meta, "sources": [dict(zip(("source", "path", "status", "requests"), s)) for s in status],
                   "rows": flat}, fh, indent=1)
    with open(os.path.join(outdir, "usage.csv"), "w", newline="", encoding="utf-8") as fh:
        w = csv.DictWriter(fh, fieldnames=list(flat[0]) if flat else ["tool"])
        w.writeheader()
        w.writerows(flat)
    md = render_markdown(meta, status, usage.rows)
    with open(os.path.join(outdir, "report.md"), "w", encoding="utf-8") as fh:
        fh.write(md)
    if args.print:
        print(md)
    for n, p, s, c in status:
        print(f"{n:32} {s:10} {c:>8,} requests  {p}", file=sys.stderr)
    print(f"wrote {tilde(outdir)}/{{report.md,usage.json,usage.csv}}", file=sys.stderr)


if __name__ == "__main__":
    main()
