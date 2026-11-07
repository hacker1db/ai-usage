# foundry-entra-proxy

A loopback-only proxy that lets OpenAI-compatible clients (Codex CLI, CLIProxyAPI) call the GPT
deployments on an **Entra-only** Azure AI Foundry resource. It replaces the .NET
`codex-azure-entra-proxy` and is written against the Go standard library only.

```
Codex CLI ───────────────────────────────► foundry-entra-proxy :8787 ──► Foundry /openai/v1 (Entra bearer)
Claude Code ─► CLIProxyAPI :8317 (Anthropic→OpenAI) ─┘
```

Entra tokens come from `az account get-access-token`; there is no Azure SDK and no
`DefaultAzureCredential` chain. The Azure CLI session is the single credential source.

## Install (macOS LaunchAgent)

`install.sh` builds the binary into `~/.local/bin`, creates the client key if it is missing, and
runs the proxy as the per-user LaunchAgent `dev.hacker1db.foundry-entra-proxy`:

```zsh
cd foundry-entra-proxy
./install.sh                               # resource from ANTHROPIC_FOUNDRY_RESOURCE, or --resource <name>
./install.sh --deployments gpt-6-luna,gpt-6.1-sol,gpt-6-astra   # default allowlist shown
./install.sh --check                       # binary, key, agent, /healthz, /readyz
./install.sh --uninstall                   # unload the agent (keeps binary, key, logs)
```

The agent sets `FEP_UPSTREAM_BASE=https://<resource>.services.ai.azure.com`, `FEP_AZ_PATH`, and a
minimal `PATH`, restarts on crash (`KeepAlive`), and logs to
`~/Library/Logs/foundry-entra-proxy/proxy.log`. Rerun `install.sh` after pulling changes. It needs a
working `az login`; `/readyz` reports `az_login_required` until then.

### Build only

```zsh
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o ~/.local/bin/foundry-entra-proxy .
```

## Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `FEP_UPSTREAM_BASE` | yes | | `https://<resource>.services.ai.azure.com` (https, no path) |
| `FEP_CLIENT_KEY_FILE` | yes¹ | | File holding the client-facing key (≥32 chars, mode `600`) |
| `FEP_CLIENT_KEY` | yes¹ | | Client key from the environment, used only when no file is set |
| `FEP_ALLOWED_DEPLOYMENTS` | yes | | Comma-separated Foundry deployment names clients may use |
| `FEP_LISTEN` | no | `127.0.0.1:8787` | Must be a loopback address |
| `FEP_TOKEN_SCOPE` | no | `https://ai.azure.com/.default` | Scope passed to `az account get-access-token` |
| `FEP_TENANT` | no | | Passed as `--tenant` |
| `FEP_AZ_PATH` | no | `az` | Azure CLI path (set it for LaunchAgents with a minimal `PATH`) |
| `FEP_STREAM_IDLE_TIMEOUT` | no | `2m` | Cuts a streamed response with no bytes for this long |
| `FEP_RESPONSE_HEADER_TIMEOUT` | no | `10m` | Maximum wait for Foundry's response headers |
| `FEP_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |

¹ One of the two. The client key only authenticates local clients; it is never sent upstream.

Generate a key outside any synced directory (not `~/.localrc`, which may be OneDrive-backed):

```zsh
mkdir -p ~/.config/foundry-entra-proxy && chmod 700 ~/.config/foundry-entra-proxy
(umask 077; openssl rand -hex 32 > ~/.config/foundry-entra-proxy/client.key)
```

## Codex CLI

Codex 0.166+ profiles are overlay files: `codex -p foundry` layers `$CODEX_HOME/foundry.config.toml`
on top of `~/.codex/config.toml`. Plain `codex` never reads the overlay, so a ChatGPT login in the
base config keeps working unchanged. Don't use the legacy `[profiles.foundry]` table or
`profile = "foundry"` in `config.toml`: Codex 0.166 refuses to start with `-p foundry` while either
is present.

```toml
# ~/.codex/foundry.config.toml
model          = "gpt-6.1-sol"   # a deployment on FEP_ALLOWED_DEPLOYMENTS
model_provider = "foundry"

[model_providers.foundry]
name     = "Azure AI Foundry via foundry-entra-proxy"
base_url = "http://127.0.0.1:8787/openai/v1"
env_key  = "FOUNDRY_PROXY_KEY"
wire_api = "responses"
```

```zsh
FOUNDRY_PROXY_KEY="$(<~/.config/foundry-entra-proxy/client.key)" codex -p foundry
```

The dotfiles `codex-foundry` launcher does this for you.

## Behavior

- **Loopback only:** startup rejects non-loopback `FEP_LISTEN`, and each request's peer must be loopback.
- **Client auth:** `Authorization: Bearer`, `api-key`, or `x-api-key`, compared in constant time.
- **Header stripping:** `Authorization`, `api-key`, `x-api-key`, `Proxy-Authorization`, and
  `Cookie` are removed before the Entra bearer is attached.
- **Route allowlist:** only these routes are forwarded; the model must be on `FEP_ALLOWED_DEPLOYMENTS`.

  | Method | Path under `/openai/v1/` | Model check |
  |---|---|---|
  | POST | `responses`, `responses/compact`, `responses/input_tokens`, `chat/completions`, `embeddings` | required in body |
  | GET | `models` | none |
  | GET | `models/{deployment}` | path |
  | GET, DELETE | `responses/{id}` | none |
  | GET | `responses/{id}/input_items` | none |
  | POST | `responses/{id}/cancel` | none |

- **Tokens:** cached until 5 minutes before expiry. An upstream `401` invalidates the cache, re-runs
  `az` once, and retries; a second `401` is returned with `X-Proxy-Error: upstream_unauthorized`.
- **Streaming:** SSE is flushed as it arrives. Request cancellation propagates upstream.
- **Logs:** JSON on stderr with a correlation ID, method, path, model, status, Foundry request ID,
  bytes, duration, and `client` (the User-Agent product token, such as `codex_cli_rs`). Headers,
  bodies, and key material are never logged.
- **Token usage:** for 2xx responses the proxy reads `usage` as the body streams through (a JSON
  body, the Responses `response.completed` event, or a Chat Completions usage chunk) and adds
  `input_tokens`, `output_tokens`, `cached_tokens`, `reasoning_tokens`, and `total_tokens` to the
  request log line. The body is passed through unchanged. The proxy drops the client's
  `Accept-Encoding` so the upstream hop is decoded by the transport; the loopback hop is
  uncompressed. Bodies or SSE lines over 16 MiB pass through without usage.

  ```zsh
  # Tokens per model today
  jq -s --arg d "$(date +%F)" 'map(select(.msg=="request" and .input_tokens and (.time|startswith($d))))
    | group_by(.model) | map({model: .[0].model, requests: length,
      input: (map(.input_tokens)|add), output: (map(.output_tokens)|add)})' \
    ~/Library/Logs/foundry-entra-proxy/proxy.log
  ```
- **Correlation:** every response carries `X-Correlation-ID`; Foundry receives it as
  `x-ms-client-request-id`.

### Endpoints and errors

`GET /healthz` reports that the process is up. `GET /readyz` acquires a token and returns `503`
when `az` cannot. Neither needs the client key.

Errors raised by the proxy carry an `X-Proxy-Error` header and an OpenAI-style JSON body, so they
can be told apart from Foundry's own responses:

| Status | `X-Proxy-Error` | Meaning |
|---|---|---|
| 401 | `client_unauthorized` | Missing or wrong client key |
| 401 | `upstream_unauthorized` | Foundry rejected a freshly acquired token (RBAC or scope) |
| 403 | `non_loopback` | Peer is not loopback |
| 403 | `route_not_allowed` | Method or path is not on the allowlist |
| 403 | `deployment_not_allowed` | Model is not on `FEP_ALLOWED_DEPLOYMENTS` |
| 400 | `model_required`, `invalid_json` | The request body cannot be checked |
| 413 | `request_too_large` | Body over 64 MiB |
| 502 | `upstream_unreachable` | Foundry could not be reached |
| 503 | `az_login_required` | Run `az login` |
| 503 | `token_unavailable` | `az account get-access-token` failed for another reason |
| 504 | `upstream_timeout` | No response headers within `FEP_RESPONSE_HEADER_TIMEOUT` |

## Development

```zsh
go vet ./...
go test -race ./...
go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^foundry-entra-proxy'
```

The last command prints nothing while the module uses only the standard library.
