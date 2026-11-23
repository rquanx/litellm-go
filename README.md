# minato-runtime

Low-memory Go data plane for Minato.

The Go port tracks the public LiteLLM Python implementation by release. The compatibility baseline and upstream comparison workflow are documented in [LiteLLM Python port](upstream/README.md). The Minato repository defines the full migration strategy, including the boundary between Go and the optional LiteLLM compatibility layer.

The runtime receives versioned route snapshots from Minato and sends OpenAI-compatible requests to CPA, OpenCode, or another provider. It keeps no project database and exposes only runtime, health, and route-management endpoints.

## Run locally

```bash
MINATO_RUNTIME_TOKEN=local-token go run ./cmd/minato-runtime
```

Publish a route snapshot to `PUT /internal/v1/routes/{alias}` with the runtime token. The snapshot contains ordered upstream candidates and their credentials. The runtime chooses the next candidate only for retryable failures such as `429`, connection errors, timeouts, and upstream `5xx` responses. Client `400` and `422` errors return immediately.

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

For PostgreSQL storage, set `general_settings.database_url` and omit the local state flags. The Go proxy uses its own `minato_proxy_state` table, and encrypts model state using `MINATO_RUNTIME_STATE_KEY` or the proxy master key. On a copy of a LiteLLM database, `--import-litellm-models` copies compatible model deployments into empty Go state. Run `--import-litellm-keys` separately to import model-scoped keys without changing the token presented by clients. Preserve `LITELLM_SALT_KEY` during model import if the source deployment used a separate salt.

Use `--import-litellm-access` in place of the key-only import when keys belong to teams, internal users or organizations. The command reads a consistent PostgreSQL snapshot and commits only when the imported restrictions can be represented. Supported team-member model scopes are preserved from LiteLLM budget records. Active monetary budgets and unsupported roles or policy bindings cause the entire access import to fail.

In standalone PostgreSQL mode, `/spend/logs/v2` shows inference metadata and token counts. `/global/spend/logs` shows daily totals. The cost estimate uses configured or pinned input and output token prices, and missing prices produce a null cost. Spend records contain metadata and omit prompt and response content. Set `general_settings.disable_spend_logs: true` to disable this storage.

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
MINATO_RUNTIME_TOKEN=local-token docker compose up --build
```

The image is built as a static Go binary and runs as a non-root user. The default listener is `0.0.0.0:4300`.

`MINATO_RUNTIME_MAX_CONCURRENT` controls the maximum number of in-flight upstream requests and defaults to `128`.

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
