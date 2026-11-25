# litellm-go

Low-memory Go gateway for deployments that use LiteLLM Proxy APIs and configuration. Management and provider APIs are enabled by default.

The Go proxy tracks public LiteLLM `v1.85.0` behavior. It exposes one generic Runtime with LiteLLM-compatible configuration. Use the API and provider switches, native path selection and YAML settings to shape a Minato deployment instead of selecting a separate mode. PostgreSQL and Redis features are available when configured. See the coverage table for verified behavior and open compatibility gaps. [LiteLLM compatibility](upstream/README.md)

## Standalone quick start

```bash
LITELLM_MASTER_KEY=local-dev-key docker compose up --build
curl -H 'Authorization: Bearer local-dev-key' http://127.0.0.1:4000/v1/models
```

The proxy starts with an empty model list. Supply a LiteLLM-compatible YAML file with `--config` or add a model through `POST /model/new`. Set `--model-state` and `--key-state` or configure PostgreSQL if management changes should persist across restarts. Standalone mode uses port 4000 by default and requires a master key. Public releases are built for Linux, macOS and Windows on amd64 and arm64.

## API configuration

| Environment variable | Default | Effect |
| --- | --- | --- |
| `LITELLM_GO_MANAGEMENT_API` | `true` | Exposes model, key, policy and other management APIs |
| `LITELLM_GO_PROVIDER_APIS` | `true` | Enables additional provider and resource routes, pass-through, and LiteLLM path aliases. Core inference and model discovery keep their LiteLLM response formats when disabled. |
| `LITELLM_GO_ROUTE_PUBLICATION_TOKEN` | Unset | Enables authenticated publication of route snapshots through `/internal/v1/routes/{alias}` |
| `LITELLM_GO_PORT` | `4000` | Sets the HTTP listener port |
| `LITELLM_GO_CONFIG` | Unset | Loads a LiteLLM YAML configuration |
| `LITELLM_GO_MODEL_STATE` | Unset | Persists dynamic models to a local file |
| `LITELLM_GO_KEY_STATE` | Unset | Persists virtual keys to a local file |
| `LITELLM_GO_MAX_CONCURRENT` | `128` | Caps in-flight upstream inference requests |
| `LITELLM_GO_STATE_KEY` | Proxy master key | Encrypts managed configuration in PostgreSQL |
| `LITELLM_GO_COMPATIBILITY_UPSTREAM` | Unset | Uses an existing LiteLLM Proxy HTTP or HTTPS origin for compatibility requests |
| `LITELLM_GO_NATIVE_PATHS` | Unset | Comma-separated exact paths or `/prefix/*` patterns to serve in Go when a compatibility upstream is configured |
| `LITELLM_GO_DASHBOARD_DIR` | Unset | Serves an existing LiteLLM Dashboard export from `/ui/` |

For example, an external controller can publish route snapshots to a proxy with the core inference endpoints enabled.

```bash
LITELLM_GO_MANAGEMENT_API=false LITELLM_GO_PROVIDER_APIS=false \
LITELLM_GO_ROUTE_PUBLICATION_TOKEN=publisher-token LITELLM_MASTER_KEY=inference-token \
LITELLM_GO_PORT=4300 go run ./cmd/minato-runtime
```

Send a versioned snapshot through `PUT /internal/v1/routes/{alias}` using the publication token. Use the master key or a scoped virtual key for inference. The publication API accepts ordered upstream candidates and their credentials. The runtime tries the next candidate after eligible failures such as `429`, connection errors, timeouts and upstream `5xx` responses. Client `400` and `422` errors return immediately.

### Serve the LiteLLM Dashboard from Go

The default Docker build includes the pinned LiteLLM static UI. The `runtime` build target produces the smaller API-only image. The Go process serves the files and handles authentication and management APIs. Open `/ui/` after starting the container. Sign in with `UI_USERNAME` and `UI_PASSWORD`, which default to `admin` and the proxy master key. Enable PostgreSQL to persist management changes.

```bash
docker build --target dashboard -t litellm-go:dashboard .
docker run --rm -p 127.0.0.1:4000:4000 -e LITELLM_MASTER_KEY=local-dev-key litellm-go:dashboard
```

The binary also accepts a Dashboard export mounted through `LITELLM_GO_DASHBOARD_DIR`. The compatibility coverage records the verified Dashboard flows.

### Use the pinned LiteLLM backend for compatibility

Set `LITELLM_GO_COMPATIBILITY_UPSTREAM=http://litellm:4000` to expose an existing LiteLLM Proxy through the same gateway listener. With an empty native path list, the upstream handles all requests, including credentials, Dashboard, streaming and resource operations. The gateway starts with just the upstream origin in this configuration. Set `LITELLM_GO_NATIVE_PATHS` to opt individual paths into Go. Keep operations that share keys, model configuration or resource IDs on the same backend. The gateway selects the handler before processing the request and returns that handler's status and response.

If you enable the route publication API alongside a compatibility upstream, include `/internal/v1/routes/*` in the native path list.

Prefix an entry with `!` to give the compatibility upstream priority for that path. For example, `/guardrails/*,!/guardrails/test_custom_code` keeps native guardrail APIs in Go and sends custom Python code tests to LiteLLM. The upstream authenticates those requests with its own proxy credentials.

To launch the pinned backend alongside the gateway, set `LITELLM_COMPAT_CONFIG` to its LiteLLM YAML file and provide `LITELLM_MASTER_KEY`. The compatibility Compose stack includes PostgreSQL for database-backed management features. Set `LITELLM_COMPAT_DATABASE_URL` to use an external PostgreSQL instance. The Python service is reachable through the gateway's existing local port 4000.

```bash
docker compose -f compose.yaml -f compose.compatibility.yaml up -d --build
```

Use `credentialRef` for deployed routes so Runtime resolves credentials from its own environment. A candidate can also use `apiKey` for local development.

```json
{
  "alias": "minato-feature-1",
  "version": "1",
  "candidates": [{
    "sourceId": "primary",
    "modelId": "primary-model",
    "apiBase": "http://provider/v1",
    "credentialRef": "MINATO_UPSTREAM_PRIMARY",
    "endpoint": "responses"
  }]
}
```

The runtime supports JSON and SSE for `/v1/responses` and `/v1/chat/completions`. Requests can use either protocol upstream and are normalized back to the client protocol, including streaming text deltas and completion events.

Successful responses include `X-Minato-Runtime-Source`, `X-Minato-Runtime-Model` and `X-Minato-Runtime-Attempts` headers. Minato uses these headers to associate the final selected upstream with its audit record.

The native server publishes its OpenAPI document at `/openapi.json`. The document includes operations whose schemas are drawn from the pinned LiteLLM release and respects the management and provider API switches.

## Run with a LiteLLM Proxy config

The Go runtime can start directly from a LiteLLM YAML file. The supported fields cover `model_list`, `model_info.supported_endpoints`, `general_settings.master_key`, direct `fallbacks`, `timeout` and `num_retries` from 0 to 8. OpenAI-compatible deployments use an explicit `api_base` and bearer credential. Anthropic and Gemini deployments can use their default public API bases. Credentials can use LiteLLM's `os.environ/VARIABLE` syntax. Unsupported configuration fields produce a startup error.

```bash
go run ./cmd/minato-runtime --config proxy.yaml --host 127.0.0.1 --port 4000
```

Standalone mode supports dynamic deployments through `/model/info`, `/model/new`, `/model/update`, `PATCH /model/{model_id}/update` and `/model/delete`. `--model-state PATH` persists these deployments to a local JSON file. Use credential references in dynamic deployments to keep provider secrets in the environment.

The master key can issue, inspect and revoke model-scoped virtual keys through `/key/generate`, `/key/info` and `/key/delete`. It can also list keys, change their scopes and validity, or temporarily block them through `/key/list`, `/key/update`, `/key/block` and `/key/unblock`. Use `--key-state PATH` to persist hashed virtual keys, teams and access settings. Use distinct paths for the model and key state files.

`/key/regenerate` rotates a virtual key. A `grace_period` keeps the old key valid for a bounded interval. During that interval the old key follows the replacement key's scopes and block status.

The master key can also create teams at `/team/new` and bind new virtual keys with `team_id`. Bound keys use the intersection of their own model list and the team's list. A key with `models: ["all-team-models"]` follows the team's model list. Changes made through `/team/update`, `/team/block` and `/team/unblock` apply to subsequent requests. `/team/delete` revokes the team's keys, including when another team is later created with the same ID.

Use `/organization/new` to create an organization, then assign its `organization_id` to a team or virtual key. The organization's model list further restricts the models that a key can use. Use `/organization/info`, `/organization/list` and `/organization/update` to view and modify organizations. Deleting an organization also removes its teams and revokes their keys and any keys directly bound to the organization.

Create an internal user with `/user/new` and use the returned key or assign the `user_id` when generating a key separately. User-bound keys follow the user's model list alongside their other model scopes. Changing the role to `internal_user_viewer` disables inference with those keys. Deleting the user revokes their keys.

For PostgreSQL storage, set `general_settings.database_url` and omit the local state flags. The Go proxy uses its own `minato_proxy_state` table and encrypts model state using the configured state key. On a copy of a LiteLLM database, `--import-litellm-models` copies compatible model deployments into empty Go state. Run `--import-litellm-keys` separately to import model-scoped keys without changing the token presented by clients. Preserve `LITELLM_SALT_KEY` during model import if the source deployment used a separate salt.

Use `--import-litellm-access` in place of the key-only import when keys belong to teams, internal users or organizations. The command reads a consistent PostgreSQL snapshot and commits only when the imported restrictions can be represented. Supported team-member model scopes and monetary budgets are preserved. For key or team `budget_limits`, set `LITELLM_GO_IMPORT_REDIS_URL` to the source LiteLLM Redis URL and set `LITELLM_GO_IMPORT_REDIS_NAMESPACE` when the source cache uses a namespace. Stop source traffic and flush spend writes before importing. Inference uses the imported counter until its first reset and adds new usage to that balance.

In standalone PostgreSQL mode, `/spend/logs/v2` shows inference metadata and token counts. `/global/spend/logs` shows daily totals. The cost estimate uses configured or pinned input and output token prices, and missing prices produce a null cost. By default, spend records contain accounting metadata. With PostgreSQL, opt in to prompts and responses with `general_settings.store_prompts_in_spend_logs: true` or `STORE_PROMPTS_IN_SPEND_LOGS=true`. Authorized callers retrieve captured content through `/spend/logs/ui/{request_id}`. Go caps each captured payload at 64 KiB and stores bounded SSE JSON events when a completed response object is unavailable. Set `general_settings.disable_spend_logs: true` to disable spend logging.

```yaml
model_list:
  - model_name: summarize
    litellm_params:
      model: openai/chat_completions/example-model
      api_base: https://provider.example/v1
      api_key: os.environ/UPSTREAM_KEY
general_settings:
  master_key: os.environ/PROXY_MASTER_KEY
router_settings:
  timeout: 30
  num_retries: 0
```

## Run with Docker

```bash
docker build -t litellm-go .
docker run --rm -p 127.0.0.1:4000:4000 -e LITELLM_MASTER_KEY=local-dev-key litellm-go
```

The image is built as a static Go binary and runs as a non-root user.


## API surface

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/responses`
- Standalone native Responses retrieval, deletion, input-item listing, and cancellation
- `POST /v1/chat/completions`
- `POST /v1/embeddings`
- `POST /v1/completions` and `POST /v1/moderations`
- `POST /v1/rerank` for Cohere and Jina
- `POST /v1/images/generations` and `POST /v1/images/edits`
- `POST /v1/audio/speech`, `POST /v1/audio/transcriptions` and `POST /v1/audio/translations`
- `POST /v1/messages` and `POST /v1/messages/count_tokens` for Anthropic
- `POST /v1beta/models/{model}:generateContent` and `POST /v1beta/models/{model}:streamGenerateContent` for Gemini
- Bedrock Converse via Chat Completions and Responses with static AWS credentials, session tokens, or a Bedrock bearer token
- Standalone `/v1/files` and `/v1/batches` for OpenAI and Azure deployments. Set `target_model_names` to one alias when uploading to a proxy with multiple eligible deployments. The Go proxy preserves deployment affinity for batch input and output files using its own resource ID format.
- `PUT /internal/v1/routes/{alias}`
- `DELETE /internal/v1/routes/{alias}`

Minato manages projects, environments, route versions, audit records, replay and usage attribution. A Minato route update replaces the in-memory snapshot for one alias. In standalone mode, native Responses returns a deployment-tagged response ID for resource operations and follow-up calls. Azure deployments can specify a dated `api_version` or use an explicit `/openai/v1` base.
