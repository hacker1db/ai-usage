# ai-gateway

A loopback API gateway that gives local tools one endpoint and one gateway-issued API key, then
forwards their calls to your upstream accounts. It speaks both wire formats tools expect, the
OpenAI Chat Completions API and the Anthropic Messages API, and translates between them when the
caller and the upstream differ. Every call is logged as a usage line that `ai-usage` reads, so
gateway traffic shows up in reports and on the dashboard (the **Gateway** tab) next to Claude Code,
Codex, and Copilot.

```text
Caido / scripts / editors                      ai-gateway :8789                    upstream accounts
  OpenAI format   ─► /v1/chat/completions ─┐                       ┌─► Azure AI Foundry (az CLI Entra token)
  Anthropic format ─► /v1/messages ────────┼─► key check, model ───┼─► GitHub Models (gh auth token)
                     /v1/models ───────────┘   allowlist, translate ├─► Anthropic / OpenAI API keys
                                               usage log            └─► another loopback proxy
```

It uses the Go standard library only, binds loopback only, and holds no secrets in its config:
client keys are stored as SHA-256 hashes, and upstream credentials are references.

## Quick start

```zsh
make build                                   # bin/ai-gateway
mkdir -p ~/.config/ai-gateway && chmod 700 ~/.config/ai-gateway
cp ai-gateway.example.json ~/.config/ai-gateway/config.json && chmod 600 ~/.config/ai-gateway/config.json

bin/ai-gateway keygen -name caido            # prints the key once, plus the config entry
# paste {"name": "caido", "key_sha256": "..."} into "clients", edit providers and models

bin/ai-gateway check                         # validates the config and resolves every credential
bin/ai-gateway                               # serves on http://127.0.0.1:8789
```

`-config` (or `AI_GATEWAY_CONFIG`) points at another config file. `keygen -key-file PATH` writes
the key to a new mode-600 file instead of printing it.

## Configuration

The config is JSON. Unknown fields are rejected, and the file must not be writable by group or
others, because a `command` credential runs what the file names.

| Field | Default | Purpose |
|---|---|---|
| `listen` | `127.0.0.1:8789` | Must be a loopback address |
| `log_file` | stderr | Usage log, appended with mode 600. `ai-usage` reads `~/Library/Logs/ai-gateway/gateway.log` by default (`-gateway-log` to change) |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `max_request_bytes` | 32 MiB | Request body limit |
| `max_concurrent_requests` | `64` | Requests in flight at once; more get 503 with `Retry-After: 1` |
| `allowed_origins` | none | Exact browser origins allowed to call the gateway with CORS, such as `http://localhost:5173`. No `*` or `null`; `http` only for loopback hosts. Leave empty unless a web app must call the gateway |
| `stream_idle_timeout` | `2m` | Cuts an upstream stream with no bytes for this long |
| `response_header_timeout` | `10m` | Maximum wait for upstream response headers |
| `clients[]` | required | `name`, `key_sha256`, optional `models` allowlist (aliases) |
| `providers.<name>` | required | One upstream account, see below |
| `models.<alias>` | required | `provider`, optional upstream `model` (defaults to the alias) |

### Providers

| Field | Purpose |
|---|---|
| `kind` | `openai` (Chat Completions; the gateway posts to `base_url` + `/chat/completions`) or `anthropic` (Messages; `base_url` + `/v1/messages`) |
| `base_url` | `https://...`, or `http://` to a loopback host only. No credentials, query, or fragment |
| `auth.type` | `azcli`, `command`, `file`, `env`, or `none` |
| `auth.header` | `authorization` (Bearer, default), `x-api-key` (Anthropic API), or `api-key` (Azure keys) |
| `headers` | Extra static headers. Auth, cookie, and host headers are refused |
| `max_tokens_field` | `openai` kind: `max_completion_tokens` (default) or `max_tokens`, for translated requests |
| `anthropic_version` | `anthropic` kind: default `anthropic-version` header (`2023-06-01`) |

Credential sources:

- **`azcli`**: `az account get-access-token --scope <scope>` (default `https://ai.azure.com/.default`;
  `tenant`, `az_path` optional), cached until five minutes before expiry. This is the same token
  code `foundry-entra-proxy` uses, so a working `az login` is the only credential.
- **`command`**: an argv list run without a shell, first output line used, cached for `ttl`
  (default `5m`). Examples: `["gh", "auth", "token"]` for GitHub Models, or a macOS Keychain lookup
  `["security", "find-generic-password", "-s", "<service>", "-w"]`.
- **`file`**: a key file that must be mode 600 (re-read on each call).
- **`env`**: an environment variable of the gateway process.

On an upstream 401 the gateway drops the cached credential, fetches a fresh one, and retries once.

### Example

[`ai-gateway.example.json`](../ai-gateway.example.json) configures work Foundry (GPT and Claude
deployments through Entra), GitHub Models through `gh`, Anthropic and OpenAI API keys from the
Keychain and a file, and chaining to a running `foundry-entra-proxy`. A unit test keeps it valid.

## Routes

| Route | Caller format | Notes |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI | Streaming and non-streaming |
| `POST /v1/messages` | Anthropic | Streaming and non-streaming |
| `POST /v1/messages/count_tokens` | Anthropic | Forwarded to `anthropic` providers; an estimate (bytes ÷ 4) for `openai` ones |
| `GET /v1/models` | both | The aliases this key may use, in a body both SDKs parse |
| `GET /healthz` | | No key needed, not logged |

The `/v1` prefix is optional, so a base URL with or without `/v1` works. Keys are accepted as
`Authorization: Bearer`, `x-api-key`, or `api-key`. A model named with one LiteLLM-style prefix
(`openai/<alias>`, `anthropic/<alias>`) resolves to `<alias>` when no exact alias matches.

When the caller and the provider speak the same format the request is passed through with only
the `model` rewritten (unknown fields are kept). For OpenAI streams the gateway asks for the final
usage chunk so the call can be costed, and hides it again from a caller that did not ask for it.

## Using it from Caido

Caido's AI settings take an OpenAI key and an optional OpenAI base URL ([Caido AI
docs](https://docs.caido.io/app/guides/ai), [custom-provider tutorial](https://docs.caido.io/app/tutorials/litellm)):

1. `ai-gateway keygen -name caido`, add the entry to `clients` (optionally with a `models`
   allowlist), and restart the gateway.
2. In Caido: account button → **Settings** → **AI**. Paste the gateway key into **OpenAI API
   Key** and `http://127.0.0.1:8789/v1` into **OpenAI Base URL (optional)**.
3. In Shift: **Models** → **OpenAI** → **+ Add Custom Model**, alias of your choice, **Model ID**
   `openai/<gateway alias>` (for example `openai/claude-sonnet-5-5`; any alias works, including
   Claude models, which the gateway translates).

Caido must run on the same machine: the gateway only accepts loopback connections. Each call then
appears in `ai-usage` under tool `ai-gateway`, profile = provider name, model = upstream model.

## Other clients

- **OpenAI SDKs and OpenAI-compatible tools**: base URL `http://127.0.0.1:8789/v1`, API key = a
  gateway key.
- **Anthropic SDKs**: base URL `http://127.0.0.1:8789`, API key = a gateway key.
- **Claude Code against non-Claude models**: `ANTHROPIC_BASE_URL=http://127.0.0.1:8789` and
  `ANTHROPIC_AUTH_TOKEN=<gateway key>`. Translation covers text, tools, images and streaming (see
  limits below); test long agentic sessions before relying on it.

## Translation limits

Translation maps the portable subset: system prompts, text, images (base64 and URL), function
tools and tool results, `tool_choice`, stop sequences, temperature/top_p, max tokens, streaming,
and usage (including cached tokens). Dropped, because the other side has no equivalent: Anthropic
thinking blocks, server tools (web search, code execution), documents, and cache-control hints;
OpenAI `n > 1`, logprobs, response formats, and audio. Images inside tool results are dropped.
When an OpenAI request has no max tokens, Anthropic's required `max_tokens` is set to 8192.

## Usage, cost, and logs

Each call writes one JSON line (`msg: "request"`) with the client name, alias, provider, upstream
model, status, translation flag, duration, and token counts. It never contains a client key or an
upstream credential. `ai-usage` counts lines with a 2xx status toward totals (unlike the Foundry
proxy cross-check, gateway callers leave no other logs) and prices them by upstream model; unknown
models show tokens without a price.

## Security model

- Loopback bind and loopback peer check.
- Browser pages are refused: the gateway is for local programs, but any web page can send
  requests to `127.0.0.1`.
  - `Host` must name a loopback address (`localhost`, `*.localhost`, `127.0.0.1`, `[::1]`); anything
    else gets 421. This blocks DNS rebinding.
  - A request with an `Origin` header is refused (403 `origin_not_allowed`) unless the exact origin
    is in `allowed_origins`. A request with no `Origin` but a `Sec-Fetch-Site` other than `none` is
    refused too (403 `cross_site`).
  - CORS preflights are answered only for allowed origins, only for `GET`/`POST`, and only for the
    headers the OpenAI and Anthropic SDKs send. `Access-Control-Allow-Credentials` is never set.
  - `POST` bodies must be `application/json` (415 otherwise), so no form or `text/plain` request
    reaches a handler.
  - Every response sets `nosniff`, `no-store`, `X-Frame-Options: DENY`, a `default-src 'none'`
    CSP, `Cross-Origin-Resource-Policy: same-origin`, and `Vary: Origin`. Buffered upstream
    responses are always sent as `application/json`, whatever type the upstream claims.
- Caller `Anthropic-Version` must look like a date, and `Anthropic-Beta` tokens must be plain
  `[a-z0-9.-]` names, or they are dropped.
- A failing `command` credential's stderr is not logged, in case the helper echoed a secret.
- Client keys: 32 random bytes (`aigw_` prefix for leak scanners), stored only as SHA-256, compared
  in constant time against every client. Each client can be limited to a model allowlist.
- The request's top-level `model` must be an exact-case key; case variants are refused, so a
  case-insensitive upstream parser cannot read a different value than the one checked.
- Client `Authorization`, `x-api-key`, `api-key`, and cookies never reach an upstream; the gateway
  builds upstream requests from scratch. Upstream credential errors are logged, not returned.
- Config and key files must not be group/other writable (config) or readable (keys).

### Subscriptions (Claude Pro/Max, ChatGPT, Copilot plans)

There is deliberately no provider that reuses a consumer subscription's OAuth token (the Claude
Code or Codex CLI login, or Copilot's editor token). Those tokens are issued for the vendors' own
clients, and their terms restrict other uses. Subscription usage is already measured by `ai-usage`
from the CLIs' own logs. Route tooling through API keys, Foundry, or GitHub Models instead.

## Tests

- `make test` and `make coverage`: unit tests for translation, streaming, config, credentials, and
  the handler against fake upstreams; `internal/gateway` has an 85% statement-coverage floor.
- `make test-integration`: builds `ai-gateway` and `ai-usage`, runs the gateway as a process on a
  real port against fake OpenAI and Anthropic upstreams, checks passthrough and translation in both
  directions, and checks that `ai-usage report` counts the calls. With
  `npm ci --prefix test/sdk` first it also drives the gateway with the official OpenAI and
  Anthropic Node SDKs (`test/sdk/compat.mjs`). CI runs both and keeps the logs as artifacts.
