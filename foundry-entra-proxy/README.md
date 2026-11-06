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

## Build

```zsh
cd foundry-entra-proxy
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o ~/bin/foundry-entra-proxy .
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

```toml
[model_providers.foundry]
name = "Azure AI Foundry via foundry-entra-proxy"
base_url = "http://127.0.0.1:8787/openai/v1"
env_key = "FOUNDRY_PROXY_KEY"
wire_api = "responses"

[profiles.foundry]
model_provider = "foundry"
model = "<deployment name>"
```

```zsh
FOUNDRY_PROXY_KEY="$(<~/.config/foundry-entra-proxy/client.key)" codex --profile foundry
```

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
  bytes, and duration. Headers, bodies, and key material are never logged.
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
