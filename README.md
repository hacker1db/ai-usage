# ai-usage

Tracks local AI token usage across Claude Code, Codex CLI, GitHub Copilot CLI, and
foundry-entra-proxy. It is one Go module that uses only the standard library and has no build step
beyond `go build`:

| Binary | What it does |
|---|---|
| `ai-usage serve` | Live web dashboard on a loopback address (default <http://127.0.0.1:8790>) |
| `ai-usage tui` | The same dashboard in the terminal: braille line charts, block-stacked bars, heatmap |
| `ai-usage report` | Dated `report.md`, `usage.json`, `usage.csv` per tool, profile, model, and day |
| `foundry-entra-proxy` | Loopback proxy for Entra-only Azure AI Foundry deployments ([docs](docs/foundry-entra-proxy.md)) |

Every tool's logs are read read-only. Only counters, model names, timestamps, session ids, and
(for session detail) redacted titles, prompts, file paths, and commands are kept.

```zsh
make build                     # bin/ai-usage and bin/foundry-entra-proxy
make check                     # gofmt, vet, race tests, JS syntax check, stdlib-only rule
bin/ai-usage serve             # web dashboard in the foreground
bin/ai-usage tui               # terminal dashboard (reads the running dashboard, else collects locally)
bin/ai-usage report -days 7 -print
scripts/install-dashboard.sh   # run the dashboard as a LaunchAgent (see below)
```

## What it tracks

| Tool | Profile label | Local source (read-only) | Notes |
|---|---|---|---|
| Claude Code | `personal` | `~/.claude-personal/projects/**/*.jsonl` | Personal Claude subscription |
| Claude Code | `work-subscription` | `~/.claude-subscription/projects/**/*.jsonl` | Work Claude subscription |
| Claude Code | `work-foundry` | `~/.claude-foundry/projects/**/*.jsonl` | Claude on Azure AI Foundry |
| Claude Code | `legacy` | `~/.claude.retired-*/projects/**/*.jsonl` | The single `~/.claude` dir before the profile split |
| Claude Code | `no-profile` | `~/.claude/projects/**/*.jsonl` | Sessions launched without `CLAUDE_CONFIG_DIR`; should stay empty |
| Codex CLI | `personal-chatgpt` / `work-foundry` | `~/.codex/sessions`, `~/.codex/archived_sessions` | Profile comes from the session's `model_provider` |
| GitHub Copilot CLI | `github-copilot` | `~/.copilot/session-store.db` | Read with `/usr/bin/sqlite3 -readonly`, so there's no driver dependency |
| foundry-entra-proxy | client User-Agent | `~/Library/Logs/foundry-entra-proxy/proxy.log` | Cross-check only, **not** added to totals (overlaps Codex `work-foundry`) |

Metrics per row: sessions, requests, input, cached, output, reasoning, total. Input includes
cached and cache-write tokens for every tool, so `total = input + output`. Reasoning is a subset of
output (OpenAI and Copilot); Claude transcripts don't report it. Copilot rows also carry
`copilot_aiu` (`total_nano_aiu / 1e9`), a local estimate only.

The proxy writes its request lines through the `internal/proxylog` contract, which the collector
reads back, and a test pins the round trip. Renaming a field on one side fails the build or that
test instead of silently dropping usage.

## Dashboard (web and terminal)

Both front ends draw the same typed chart models from `internal/view`, so they always agree. Colors
come from one validated palette (`internal/palette`): fixed categorical slots per tool and profile
(colorblind-safe for adjacent pairs, light and dark), a gold-to-indigo sequential ramp for magnitude
(lightness falls monotonically), decorative accents that never encode data, and
reserved status colors that always carry an icon and a label.

- **Controls:** tab (All, Claude, Codex, Copilot, Foundry), profile (see below), metric (total, uncached = input -
  cached + output, output, requests), burn-rate window (1h/3h/6h), and range (24h hourly, 7d in
  4-hour buckets, 30d daily). On the web these are links (`/?tab=foundry&range=30d` bookmarks a
  view). In the TUI they're keys: `tab`/`1-5`, `m`, `r`, `b`, `s`, `p`/`P` profile, `f` refresh,
  `j/k` scroll, `o` ownership filter, `?` help, `q` quit.
- **Profile filter:** a chip bar under the controls narrows every chart, card, and session list
  to one profile (`?profile=claude-code:personal`) or a named group you define under `profiles:`
  in `projects.yaml` (`?profile=work`). It combines with the tab. Each chip shows its series
  colors and its usage in range.
- **Overview:** range total, today, burn rate, month (or budget period) to date, and a cumulative
  curve with the projection to the period end at the trailing 7-day average. Hover shows any hour.
- **Where the tokens went:** the top sessions by the selected metric, with their share of the range.
- **Usage by profile:** stacked buckets per profile, with a table view.
- **When you work:** a weekday × hour heatmap of the metric, with the busiest hour called out.
- **Token mix:** per profile, cache reads vs fresh input vs output, with the cache-hit rate. Cache
  reads are far cheaper than fresh input, so this shows where long sessions are reusing context.
- **Profiles:** a card per profile with sparkline, share, top models, and its budget meter.
- **Work:** projects (tokens and active time per repository, stacked by profile), a searchable
  session list, and the optional ai-work-report workstreams overlay. Click a session for its
  prompts, files changed, commands, tools, models, and Copilot refs (`/session?key=...`). Titles
  come from each tool (Claude AI titles, Copilot summaries, Codex thread names), else the first
  prompt, with secrets redacted. Active time counts 5-minute slots with activity.
- **Ownership:** each session's repository labeled `personal` or `org` (or your own labels), with
  usage, sessions, and projects per label. Pick a label to filter the Work section to it
  (`/?own=personal`, `o` in the TUI). See [Ownership](#ownership).
- **Sources:** collector status, history-store health, and a daily proxy vs Codex Foundry chart.

The web page is server-rendered HTML with inline SVG. About 200 lines of plain JS (`internal/web/static/app.js`,
`@ts-check` typed) add tooltips, the crosshair, live refresh (it polls `/api/etag` every 10 s and
swaps the page body when the snapshot changes), the session filter, and a theme toggle. The page
also works without the script. The TUI uses raw termios ioctls (darwin and linux), an alternate
screen, 24-bit color, `NO_COLOR`, and `-theme light`. Use `-once` to print a single frame, for
example `ai-usage tui -once -color never | less`.

### Ownership

A session's owner is the GitHub user or org in its repository's remote URL. The remote comes from
the repo's `.git/config` (bare-clone and worktree layouts included), or from the repository the tool
reported (Copilot, Codex).

- **Built-in rule:** if the owner is a GitHub **user** account, the repo is `personal`. If it's an
  **organization**, hosted elsewhere (such as Azure DevOps), or has no remote, it's `org`. The
  account type comes from `gh api users/<owner>` once per owner, using gh's existing login. It is
  cached in `owners.json` in the data dir for 90 days, so only new owners trigger a lookup.
  Without gh the lookup fails quietly and those repos fall back to `org`.
- **Exceptions:** add `~/.config/ai-usage/projects.yaml` (or `-projects`, `$AI_USAGE_PROJECTS`).
  See [projects.example.yaml](projects.example.yaml). Map labels to `owners`, `repos`
  (`owner/name` or `name`, `*` globs), or `paths` (folders, even ones that aren't repos). The most
  specific match wins: repos, then the longest path, then owners. Sessions that nothing matches still
  get the built-in rule unless you set `default:`, which turns it off. Up to 8 labels. Edits apply
  on the next refresh.

```yaml
labels:
  shared:
    owners: [Example-Corp-Shared]
  personal:
    paths: [~/Developer/sites]
```
The same file defines the dashboard's **profile filters**. Each entry names a group of
`tool:profile` series (or a bare profile name, matching any tool), with `*` globs, up to 12:

```yaml
profiles:
  personal: [claude-code:personal, codex:personal-chatgpt]
  work: [claude-code:work-*, codex:work-foundry, copilot:*]
  foundry:
    label: Azure Foundry
    match: [work-foundry]
```

A broken `profiles:` section only drops the profile filters (with a warning on the dashboard).
The ownership rules still apply.


The file is a YAML subset, parsed without a dependency: mappings, `- item` and `[a, b]` lists,
quoted strings, and comments. Anything else (anchors, `{}` maps, `|` blocks) is an error with a
line number. A broken file falls back to the built-in rule and shows the error on the dashboard.
Ownership totals come from sessions, so they cover the Work section; the hourly charts above it
aren't split by repository.

### Run it in the background (macOS)

```zsh
scripts/install-dashboard.sh             # build ~/.local/bin/ai-usage and (re)load the agent
scripts/install-dashboard.sh --check     # agent state + /healthz
scripts/install-dashboard.sh --uninstall
```

The LaunchAgent is labeled `<your-username>.ai-usage-dashboard`, using the account that runs the
installer, with characters launchd doesn't allow replaced by `-`. Installing retires any other
`*.ai-usage-dashboard` agent, such as the earlier Python one. The agent runs the installed binary,
so moving or re-cloning this checkout doesn't break it. Logs go to `~/Library/Logs/ai-usage/dashboard.log`.

### Files

| Path | What | Override |
|---|---|---|
| `~/.local/share/ai-usage/hourly.json` | History store | `-data-dir`, `$AI_USAGE_DATA_DIR` |
| `~/.local/share/ai-usage/work-report/` | ai-work-report overlay | `-data-dir` |
| `~/.config/ai-usage/dashboard.json` | Optional budgets ([example](dashboard.example.json)) | `-config`, `$AI_USAGE_CONFIG` |
| `~/.config/ai-usage/projects.yaml` | Optional ownership exceptions ([example](projects.example.yaml)) | `-projects`, `$AI_USAGE_PROJECTS` |
| `~/.local/share/ai-usage/owners.json` | Cached GitHub user/org answers | `-data-dir` |

The installer copies an existing `<checkout>/data/hourly.json`, `data/work-report/`, and
`dashboard.json` into those places once. The history format is unchanged from the Python version.

- **History store:** every collection merges into `hourly.json`, so usage survives Claude Code's
  transcript cleanup (`cleanupPeriodDays`) and deleted Codex rollouts. A past hour only grows: a
  live row with fewer requests than the stored one means transcripts were deleted, so the stored
  row is kept. The TUI opens the store read-only when it collects locally, so it never races the
  dashboard's writes.
- **Fast refreshes:** transcripts are parsed in parallel and cached by size and mtime, so a refresh
  re-reads only new or growing files.
- **Budgets:** set token budgets per `tool:profile` with a `day`, `week`, or `month` period.
  Profile cards then show a meter and whether the projection exceeds the budget. A tab whose
  profiles all have matching budgets shows the budget line on the overview chart.
- **ai-work-report overlay:** run the skill with its workdir set to the data dir's `work-report/`
  folder. The dashboard reads `classified.json` and `narrative.json` (Workstreams card, estimated
  hours saved, a Workstream column) and serves `ai-work-report.html` at `/report` in a CSP sandbox.

### Security

The server binds loopback only. It rejects non-loopback peers and any request whose `Host` header
isn't its own listener (DNS rebinding), and it requires a custom header for `POST /api/refresh`, so
other sites can't trigger it. It sends a strict CSP (`script-src 'self'`, `style-src 'self'`, no
inline styles: chart colors are CSS classes) and escapes every value through `html/template` or the
SVG builder. Tooltip data is rendered with `textContent` only.

## Report

```zsh
bin/ai-usage report                    # last 30 days -> reports/<today>/
bin/ai-usage report -days 7 -print     # also print the markdown
bin/ai-usage report -h
```

Flags: `-claude DIR=LABEL` (repeatable, replaces the default profile list), `-codex-home`,
`-copilot-db`, `-proxy-log`, `-out`. `reports/` is gitignored, and re-running on the same day
overwrites that day's folder. The Go port matches the earlier `collect.py` exactly on the same logs:
every daily row, every metric, and byte-identical markdown.

## Relationship to the ai-work-report skill

This repo is the deterministic numbers layer: nothing here calls a model. The skill
(`skills/ai-work-report` in `~/Developer/skills`) builds the narrative report (workstreams, time
saved, cost). The dashboard overlays its output when present. Ideas reused from its
`claude_digest.py` include per-profile `CLAUDE_CONFIG_DIR` reading, `~/`-relative paths, the secret
patterns, and keeping usage data out of git.

## Cross-checks and gaps

- `ccusage` (optional, needs network; pin versions):
  `CLAUDE_CONFIG_DIR=~/.claude-personal,~/.claude-subscription,~/.claude-foundry npx ccusage@<version> daily`.
  ccusage merges profiles, so use this repo for per-profile separation.
- Copilot premium-request billing is authoritative only in GitHub billing
  (`gh api /users/<login>/settings/billing/premium_request/usage` with the right token scope).
- Copilot in VS Code keeps no local token usage. Codex `history.jsonl` holds prompt text only and
  is not read.
