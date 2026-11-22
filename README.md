# minato-runtime

Low-memory Go data plane for Minato.

The Go port tracks the public LiteLLM Python implementation by release. The compatibility baseline and upstream comparison workflow are documented in [LiteLLM Python port](upstream/README.md).

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

The Go runtime can start directly from a LiteLLM YAML file for OpenAI-compatible providers. The supported fields cover `model_list`, `model_info.supported_endpoints`, `general_settings.master_key`, and the router's direct `fallbacks`, `timeout` and `num_retries: 0`. Use an explicit `api_base` for each deployment. Credentials can use LiteLLM's `os.environ/VARIABLE` syntax. Unsupported configuration fields produce a startup error.

```bash
go run ./cmd/minato-runtime --config proxy.yaml --host 127.0.0.1 --port 4000
```

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
- `POST /v1/chat/completions`
- `POST /v1/embeddings`
- `PUT /internal/v1/routes/{alias}`
- `DELETE /internal/v1/routes/{alias}`

Minato remains the source of truth for projects, environments, model catalogs, route versions, audit records, replay and usage attribution. This runtime is deliberately stateless apart from its in-memory route snapshot. A route update replaces the complete snapshot for one alias, making publication idempotent and allowing Minato to roll back by publishing an earlier version.
