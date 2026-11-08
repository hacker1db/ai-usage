# ai-usage

Local home for tracking AI token usage across my AI tools. One stdlib-only
Python script (`collect.py`) reads each tool's local logs read-only and writes a
report broken down by **tool + profile + model + day**.

## What it tracks

| Tool | Profile label | Local source (read-only) | Notes |
|---|---|---|---|
| Claude Code | `personal` | `~/.claude-personal/projects/**/*.jsonl` | Personal Claude subscription |
| Claude Code | `work-subscription` | `~/.claude-subscription/projects/**/*.jsonl` | Work Claude subscription |
| Claude Code | `work-foundry` | `~/.claude-foundry/projects/**/*.jsonl` | Claude on Azure AI Foundry |
| Claude Code | `legacy` | `~/.claude.retired-*/projects/**/*.jsonl` | the single `~/.claude` dir before the 2026-10-18 profile split (personal and work mixed) |
| Claude Code | `no-profile` | `~/.claude/projects/**/*.jsonl` | sessions launched without `CLAUDE_CONFIG_DIR`; should stay empty |
| Codex CLI | `personal-chatgpt` / `work-foundry` | `~/.codex/sessions`, `~/.codex/archived_sessions` | Profile comes from the session's `model_provider` (`openai` vs `foundry`) |
| GitHub Copilot CLI | `github-copilot` | `~/.copilot/session-store.db` table `assistant_usage_events` | Opened `mode=ro` |
| foundry-entra-proxy | client User-Agent | `~/Library/Logs/foundry-entra-proxy/proxy.log` | `"msg":"request"` slog lines; shown as a cross-check, **not** added to totals because it overlaps Codex `work-foundry` |

`~/.claude` (legacy) and `~/.claude.retired-*` are ignored. Missing sources are
reported as `not found`, not errors.

Metrics per row: sessions, requests, input, cached, output, reasoning, total.
Input is normalized to include cached and cache-write tokens for every tool, so
`total = input + output`. Reasoning is a subset of output (OpenAI/Copilot);
Claude transcripts don't report it. Copilot rows also carry `copilot_aiu`
(`total_nano_aiu / 1e9`), a local estimate only.

Only counters, model names, timestamps and session ids are read. Prompt and
response text is never copied into reports.

## Run

```bash
cd ~/Developer/ai-usage/main
python3 -I collect.py                 # last 30 days -> reports/<today>/
python3 -I collect.py --days 7 --print  # last 7 days, also print markdown
python3 -I collect.py --help
```

Options: `--claude DIR=LABEL` (repeatable, replaces the default profile list),
`--codex-home`, `--copilot-db`, `--proxy-log`, `--out`.

## Live dashboard

`dashboard.py` serves a live dashboard at <http://127.0.0.1:8790>. It reuses the
collectors in `collect.py` with hourly buckets, re-collects every 30 seconds in a
background thread, and the page polls for changes every 10 seconds.

```zsh
python3 -I dashboard.py                    # foreground; Ctrl-C to stop
./install-dashboard.sh                     # background LaunchAgent dev.hacker1db.ai-usage-dashboard
./install-dashboard.sh --check             # agent state + /healthz
./install-dashboard.sh --uninstall
```

- **Tabs:** All, Claude, Codex, Copilot, and Foundry (both `work-foundry` profiles).
  Bookmark a view with query parameters, for example `/?tab=foundry&range=30d`.
- **Controls:** metric (total, uncached = input - cached + output, output, requests),
  burn-rate window (1h/3h/6h), and range (24h hourly, 7d in 4-hour buckets, 30d daily).
- **Overview:** range total, today, burn rate, month (or budget period) to date, and a
  cumulative chart with the projection to the period end at the trailing 7-day average.
- **Breakdown, profiles, models, sources:** stacked usage per profile (with a table view),
  a card per profile with sparkline and share, a profile/model table, and collector status
  with the foundry-entra-proxy cross-check.
- **Budgets (optional):** copy `dashboard.example.json` to `dashboard.json` (gitignored) and
  set token budgets per `tool:profile` with a `day`, `week`, or `month` period. Profile cards
  then show percent used and whether the projection exceeds the budget, and a tab whose
  profiles all have matching budgets shows the budget line on the overview chart.
- **Security:** binds to `127.0.0.1` only, rejects requests whose `Host` header isn't the
  loopback listener (DNS rebinding), requires a custom header for `POST /api/refresh` (no
  cross-site triggering), and sends a strict CSP. The snapshot holds usage counters, model
  names, and session counts, never prompt or response text.
- **History store:** every collection is merged into `data/hourly.json` (gitignored), so usage
  survives Claude Code's transcript cleanup (`cleanupPeriodDays`, 30 days by default) and
  deleted Codex rollouts. A past hour only grows: a live row with fewer requests than the
  stored one means transcripts were deleted, so the stored row is kept. The Sources panel
  shows how many rows in the window come only from history. Delete the file to rebuild it.
- **Fast refreshes:** parsed transcripts are cached by size and mtime, so a refresh re-reads
  only new or growing files (about 0.2-0.3 s after the first full scan).
- Logs: `~/Library/Logs/ai-usage/dashboard.log`. The LaunchAgent runs `dashboard.py` from
  this checkout, so rerun `install-dashboard.sh` after moving it (for example after merging
  to `main`).

## Outputs

`reports/<YYYY-MM-DD>/` (gitignored):

- `report.md` - sources status, totals, by tool/profile, by model, by day
- `usage.json` - metadata, source status, and flat rows
- `usage.csv` - the same rows for spreadsheets

Re-running on the same day overwrites that day's folder, so older dated folders
act as snapshots.

## Relationship to the ai-work-report skill

Modeled on `skills/ai-work-report` in `~/Developer/skills`
(`feature-ai-work-report-skill` worktree). That skill builds a narrative HTML
dashboard (workstreams, time saved, cost) using `claude_digest.py`, model
classification, and `build.py`. This repo is the deterministic numbers layer:
nothing here calls a model. Ideas reused from `claude_digest.py`: per-profile
`CLAUDE_CONFIG_DIR` reading, `~/`-relative paths, and keeping usage data out of
git. Unlike the skill's default deny list, personal profiles are included here
but always labeled separately.

To feed the skill a Claude digest for a work profile (writes outside this repo):

```bash
python3 -I ~/Developer/skills/feature-ai-work-report-skill/skills/ai-work-report/scripts/claude_digest.py \
  --profile ~/.claude-foundry --days 30 --out /tmp/ai-work/claude-digest.json
```

## Cross-checks and gaps

- `ccusage` (optional, needs network; pin versions):
  `CLAUDE_CONFIG_DIR=~/.claude-personal,~/.claude-subscription,~/.claude-foundry,~/.claude.retired-20261018 npx ccusage@<version> daily`
  and `npx @ccusage/codex@<version> daily`. ccusage merges profiles, so use this
  repo for per-profile separation.
- Copilot premium-request billing is authoritative only in GitHub billing. With
  a token that has the right scope you can query
  `gh api /users/<login>/settings/billing/premium_request/usage` (or the org
  equivalent); it is not called by `collect.py`.
- Copilot in VS Code (`~/.config/github-copilot`) keeps no local token usage.
- Codex `history.jsonl` contains prompt text only and is not read.
- Do not write reports into `~/notes/SecondBrain`; if you want a copy there,
  copy `report.md` yourself.
