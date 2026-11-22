# LiteLLM Python port

The Go port follows the public Python implementation in LiteLLM. The initial reference is `v1.85.0` at the commit in [litellm.ref](litellm.ref), which also matches the revision of Minato's existing LiteLLM image. This is the source baseline for the port. Compatibility is verified by behavior and recorded per area. [LiteLLM v1.85.0](https://github.com/BerriAI/litellm/tree/v1.85.0)

| LiteLLM Python | Current Go implementation | Parity to verify |
| --- | --- | --- |
| `litellm/main.py`, `litellm/llms/openai/` | OpenAI-compatible requests and embeddings | Request mapping, response envelopes, errors and usage |
| `litellm/responses/`, `litellm/completion_extras/litellm_responses_transformation/` | Chat and Responses conversion, structured output, function tools, token usage and SSE | Eight structured-output cases match both Python releases. Go regressions cover interleaved tool arguments, terminal events and token usage. The provider-specific event inventory remains open. |
| `litellm/router.py`, `litellm/router_strategy/`, `litellm/router_utils/` | Ordered fallback, versioned routes and a retry budget with `Retry-After` support | Deployment strategies, specialized fallback policies and retry parity |
| `litellm/proxy/`, `litellm/proxy_auth/` | Static YAML, model CRUD, key generation and revocation with model scopes, and optional local state | Database-backed entities, richer authorization and configuration, plus remaining management APIs |
| `litellm/llms/anthropic/`, `litellm/llms/gemini/`, `litellm/llms/azure/` | Native generation, Anthropic token counting, a Chat and Responses bridge for Anthropic, and Azure OpenAI key authentication | Provider-specific payloads, authorization and event parity across the remaining operations |
| `litellm/responses/utils.py`, `litellm/proxy/response_api_endpoints/` | Deployment-tagged native Responses IDs, retrieval, deletion, input items, cancellation and affinity for follow-ups | Provider-specific resource handlers and state wrapping for other endpoints |
| `litellm/constants.py` | Generated list of OpenAI-compatible provider names for explicit base URLs and bearer credentials | Provider defaults and specialized adapters |
| Other Python provider and feature modules under `litellm/` | Inventory and port by module | Stateful APIs, remaining API families, callbacks and provider modules |

## Verify protocol behavior

The Go and Python checks share eight cases for structured output in both directions. They cover explicit JSON Schema, schema defaults, JSON object mode and plain text. [Responses to Chat Python transformation](https://github.com/BerriAI/litellm/blob/e58a561caa21169fb02174148444c08509ce7028/litellm/responses/litellm_completion_transformation/transformation.py#L2162) · [Chat to Responses Python transformation](https://github.com/BerriAI/litellm/blob/e58a561caa21169fb02174148444c08509ce7028/litellm/completion_extras/litellm_responses_transformation/transformation.py#L979)

Go tests also verify function tool definitions and named tool choices. For Responses to Chat, the Go adapter maps an explicit top-level function name to Chat's nested function name to preserve the caller's selected tool. The upstream helper treats that form as a generic required tool choice. [Python tool choice transformation](https://github.com/BerriAI/litellm/blob/e58a561caa21169fb02174148444c08509ce7028/litellm/responses/litellm_completion_transformation/transformation.py#L106)

Request conversion also covers text and image URL blocks and groups consecutive function calls into a single Chat assistant message before its tool results. [Python input transformation](https://github.com/BerriAI/litellm/blob/e58a561caa21169fb02174148444c08509ce7028/litellm/responses/litellm_completion_transformation/transformation.py#L360)

## Review an upstream release

```bash
git clone --no-checkout https://github.com/BerriAI/litellm.git ../litellm-upstream
git -C ../litellm-upstream fetch --tags origin
bash tools/litellm-diff.sh ../litellm-upstream v1.85.1
python tools/litellm-inventory.py ../litellm-upstream
python tools/sync-litellm-providers.py ../litellm-upstream --check
python tools/verify-litellm-formats.py ../litellm-upstream
python tools/verify-litellm-formats.py ../litellm-upstream v1.85.1
go test ./...
```

Run the script from the Go repository. It lists changed Python implementation paths, tests and shared metadata between the pinned commit and the selected tag. Use `git diff <baseline> <target> -- <path>` in the LiteLLM checkout to examine individual changes.

The inventory command reads HTTP route decorators and provider package directories from the selected Git revision. It finds 696 declared method and path pairs and 119 provider or shared package directories at `v1.85.0`. Route declarations are source anchors for parity work. Feature activation, router prefixes and provider capabilities require separate checks.

For each behavior change, record its upstream commit and Python source path, translate the relevant upstream test or run both implementations against the same simulated provider, then apply the Go change. Advance the pinned reference after reviewing the release and recording any remaining parity gaps. Keep the LiteLLM checkout and its test dependencies in the development environment so the Go deployment stays a standalone binary.

## Container memory comparison

On Docker Desktop, LiteLLM Proxy `v1.85.0` and the Go runtime started with the same YAML configuration and handled a warm-up request plus 60 sequential Chat Completions requests each. Both targeted the same synthetic upstream and returned 12 tokens per measured request. Docker reported 989.9 MiB for the Python container and 6.875 MiB for the Go container after the requests. This comparison covers one non-streaming route and measures container memory after the requests.
