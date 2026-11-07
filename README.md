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
  `CLAUDE_CONFIG_DIR=~/.claude-personal,~/.claude-subscription,~/.claude-foundry npx ccusage@<version> daily`
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
