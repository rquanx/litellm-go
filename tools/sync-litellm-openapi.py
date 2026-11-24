"""Generate a native-route OpenAPI snapshot from the pinned LiteLLM image."""

import argparse
import gzip
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
IMAGE = "ghcr.io/berriai/litellm:v1.85.0@sha256:2e8517e2bed423c50ab7e40fb1ac0a9cbe62a764e9d65161d871fb6a9bf75a2d"
SCHEMA = """
import json
from litellm.proxy.proxy_server import app
from litellm.proxy.common_utils.get_routes import GetRoutes

catalog = []
for route in app.routes:
    endpoint = getattr(route, "endpoint", None)
    if endpoint is not None:
        catalog.extend(GetRoutes.get_app_routes(route, endpoint))
    elif hasattr(route, "app") and hasattr(route, "path"):
        catalog.extend(GetRoutes.get_routes_for_mounted_app(route))
for route in catalog:
    if route["methods"] is not None:
        route["methods"] = sorted(route["methods"])
print(json.dumps({"schema": app.openapi(), "catalog": catalog}))
"""
REGISTRATION = re.compile(r'(?:\.Handle(?:Func)?\(|\bprotected\(mux, )"(GET|POST|PUT|PATCH|DELETE|OPTIONS|HEAD) (/[^"]+)"')
ENDPOINT = re.compile(r'^\s*"[^"]+"\s*:\s*\{"(/v1/[^"]+)"', re.M)
PARAMETER = re.compile(r"\{([^}:]+):[^}]+\}")

EXTRA_INFERENCE = {
    "/v1/responses/compact", "/v1/vector_stores", "/v1/videos",
}

PROVIDER = {
    ("post", "/v2/rerank"),
    ("post", "/utils/test_policies_and_guardrails"),
    ("post", "/usage/ai/chat"),
    ("get", "/health"), ("get", "/health/readiness"), ("get", "/health/liveness"),
    ("post", "/health/test_connection"),
    ("get", "/health/backlog"), ("get", "/health/latest"), ("get", "/health/history"),
    ("get", "/health/readiness/details"), ("get", "/health/license"),
    ("get", "/health/services"), ("get", "/health/shared-status"),
    ("get", "/debug/asyncio-tasks"),
    ("get", "/vertex_ai/live"),
    ("post", "/api/event_logging/batch"),
    ("get", "/robots.txt"),
    ("options", "/health/liveliness"), ("options", "/health/liveness"), ("options", "/health/readiness"),
    ("get", "/test"), ("get", "/active/callbacks"), ("get", "/settings"),
    ("post", "/utils/token_counter"), ("post", "/utils/transform_request"),
    ("get", "/model_group/info"),
    ("post", "/v1/messages/count_tokens"), ("post", "/messages/count_tokens"),
    ("get", "/v1/assistants"), ("post", "/v1/assistants"),
    ("delete", "/v1/assistants/{assistant_id}"),
    ("post", "/v1/threads"), ("get", "/v1/threads/{thread_id}"),
    ("get", "/v1/threads/{thread_id}/messages"), ("post", "/v1/threads/{thread_id}/messages"),
    ("post", "/v1/threads/{thread_id}/runs"),
    ("get", "/v1/responses/{response_id}"), ("delete", "/v1/responses/{response_id}"),
    ("get", "/v1/responses/{response_id}/input_items"), ("post", "/v1/responses/{response_id}/cancel"),
    ("get", "/v1/files"), ("post", "/v1/files"), ("get", "/v1/files/{file_id}"),
    ("delete", "/v1/files/{file_id}"), ("get", "/v1/files/{file_id}/content"),
    ("get", "/v1/batches"), ("post", "/v1/batches"), ("get", "/v1/batches/{batch_id}"),
    ("post", "/v1/batches/{batch_id}/cancel"),
    ("get", "/v1/responses"),
    ("get", "/v1/models/{model_id}"), ("get", "/models"), ("get", "/models/{model_id}"),
}

NONVERSIONED_ALIASES = {
    "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/moderations",
    "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/images/generations", "/v1/images/edits",
    "/v1/rerank", "/v1/ocr", "/v1/responses", "/v1/rag/query", "/v1/rag/ingest",
    "/v1/assistants", "/v1/assistants/{assistant_id}", "/v1/threads", "/v1/threads/{thread_id}",
    "/v1/threads/{thread_id}/messages", "/v1/threads/{thread_id}/runs",
}
CORE = {("get", "/"), ("get", "/v1/models"), ("get", "/health/liveliness"), ("get", "/routes")}
MANAGEMENT = {("get", "/cache/ping"), ("post", "/cache/delete"), ("post", "/cache/flushall"), ("get", "/cache/redis/info")}

RESOURCE_ROUTES = {
    "/containers": {
        "": "get post",
        "/{container_id}": "get delete",
        "/{container_id}/files": "get post",
        "/{container_id}/files/{file_id}": "get delete",
        "/{container_id}/files/{file_id}/content": "get",
    },
    "/vector_stores": {
        "": "get post",
        "/{vector_store_id}": "get post delete",
        "/{vector_store_id}/search": "post",
        "/{vector_store_id}/files": "get post",
        "/{vector_store_id}/files/{file_id}": "get post delete",
        "/{vector_store_id}/files/{file_id}/content": "get",
    },
    "/videos": {
        "": "get post",
        "/characters": "post",
        "/characters/{character_id}": "get",
        "/edits": "post",
        "/extensions": "post",
        "/{video_id}": "get",
        "/{video_id}/content": "get",
        "/{video_id}/remix": "post",
    },
    "/fine_tuning/jobs": {
        "": "get post",
        "/{fine_tuning_job_id}": "get",
        "/{fine_tuning_job_id}/cancel": "post",
    },
}


def operations():
    management = MANAGEMENT | {
        ("post", "/team/member_add"), ("post", "/team/member_update"),
        ("post", "/team/member_delete"),
    }
    for prefix in ("/customer", "/end_user"):
        management.update(("get", prefix + "/" + action) for action in ("info", "list"))
        management.update(("post", prefix + "/" + action) for action in ("new", "update", "block", "unblock", "delete"))
    for prefix in ("/vector_store", "/v1/vector_store"):
        management.add(("get", prefix + "/list"))
        management.update(("post", prefix + "/" + action) for action in ("new", "info", "update", "delete"))
    management.update((method, "/v1/memory/{key}") for method in ("get", "put", "delete"))
    management.add(("post", "/v1/mcp/server/oauth/session"))
    management.update((method, "/prompts/{prompt_id}") for method in ("put", "patch", "delete"))
    policy_routes = (ROOT / "internal/proxyconfig/policy_endpoints.go").read_text(encoding="utf-8")
    management.update((method.lower(), route) for method, route in
                      re.findall(r'"(GET|POST|PUT|DELETE) (/policies/[^"]+|/policies)"', policy_routes))
    management.add(("post", "/policies/test-pipeline"))
    management.add(("post", "/upload/logo"))
    passthrough = "/config/pass_through_endpoint"
    management.update((method, passthrough) for method in ("get", "post", "delete"))
    management.update({("post", passthrough + "/{endpoint_id}"), ("get", passthrough + "/team/{team_id}")})
    management.update({
        ("get", "/adaptive_router/state"),
        ("post", "/team/{team_id}/disable_logging"),
        ("get", "/claude-code/plugins/{plugin_name}"),
        ("delete", "/claude-code/plugins/{plugin_name}"),
        ("post", "/claude-code/plugins/{plugin_name}/enable"),
        ("post", "/claude-code/plugins/{plugin_name}/disable"),
    })
    management.update({
        ("get", "/v1/mcp/toolset"), ("get", "/v1/mcp/toolset/{toolset_id}"),
        ("post", "/v1/mcp/toolset"), ("put", "/v1/mcp/toolset"),
        ("delete", "/v1/mcp/toolset/{toolset_id}"),
    })
    for method, suffix in (
        ("get", ""), ("post", ""), ("put", ""), ("get", "/{server_id}"), ("delete", "/{server_id}"),
        ("post", "/register"), ("get", "/submissions"),
        ("put", "/{server_id}/approve"), ("put", "/{server_id}/reject"),
    ):
        management.add((method, "/v1/mcp/server" + suffix))
    for base in ("/v1/access_group", "/v1/unified_access_group"):
        management.update((method, base) for method in ("get", "post"))
        management.update((method, base + "/{access_group_id}") for method in ("get", "put", "delete"))
    for path in (ROOT / "internal/proxyconfig").glob("*.go"):
        if path.name.endswith("_test.go"):
            continue
        for method, route in REGISTRATION.findall(path.read_text(encoding="utf-8")):
            management.add((method.lower(), route.replace("...}", "}")))
    provider = PROVIDER | {
        ("get", "/v1/agents"), ("get", "/v1/agents/{agent_id}"), ("get", "/public/agent_hub"),
        ("get", "/mcp-rest/tools/list"), ("post", "/mcp-rest/tools/call"),
        ("get", "/v1/mcp/tools"), ("get", "/v1/mcp/access_groups"),
        ("get", "/v1/mcp/server/health"), ("get", "/v1/mcp/network/client-ip"),
        ("get", "/v1/mcp/user-credentials"),
        ("post", "/v1/mcp/server/{server_id}/oauth-user-credential"),
        ("delete", "/v1/mcp/server/{server_id}/oauth-user-credential"),
        ("post", "/v1/mcp/server/{server_id}/user-credential"),
        ("delete", "/v1/mcp/server/{server_id}/user-credential"),
        ("get", "/v1/mcp/server/{server_id}/user-credential/status"),
        ("get", "/v1/mcp/server/{server_id}/oauth-user-credential/status"),
        ("get", "/v1/skills"), ("post", "/v1/skills"),
        ("get", "/v1/skills/{skill_id}"), ("delete", "/v1/skills/{skill_id}"),
    }
    for prefix in ("anthropic", "assemblyai", "azure", "azure_ai", "cohere", "cursor", "eu.assemblyai",
                   "gemini", "langfuse", "mistral", "openai", "openai_passthrough", "vertex_ai",
                   "vertex_ai/discovery", "vllm"):
        provider.update((method, "/" + prefix + "/{endpoint}") for method in ("get", "post", "put", "patch", "delete"))
    provider.update({("post", "/bedrock/{endpoint}"), ("post", "/cursor/chat/completions"),
                     ("post", "/guardrails/apply_guardrail"), ("post", "/apply_guardrail")})
    provider.update((method, "/{mcp_server_name}/mcp") for method in ("get", "post", "put", "patch", "delete", "head", "options"))
    provider.update((method, "/toolset/{toolset_name}/mcp") for method in ("get", "post", "put", "patch", "delete", "head", "options"))
    for prefix in ("/a2a", "/v1/a2a"):
        provider.update(("post", prefix + "/{agent_id}" + suffix) for suffix in ("", "/message/send"))
        provider.update(("get", prefix + "/{agent_id}/.well-known/" + name) for name in ("agent.json", "agent-card.json"))
    for suffix, methods in {
        "": "get post",
        "/{eval_id}": "get post delete",
        "/{eval_id}/cancel": "post",
        "/{eval_id}/runs": "get post",
        "/{eval_id}/runs/{run_id}": "get post delete",
    }.items():
        provider.update((method, "/v1/evals" + suffix) for method in methods.split())
    core = set(CORE)
    for route in ENDPOINT.findall((ROOT / "internal/runtime/endpoints.go").read_text(encoding="utf-8")):
        (provider if route in EXTRA_INFERENCE else core).add(("post", route))
    provider.update((method, route[3:]) for method, route in core | provider if
                    route in NONVERSIONED_ALIASES or route.startswith(("/v1/responses/", "/v1/files", "/v1/batches")))
    provider.update((method, "/{provider}" + route) for method, route in set(provider)
                    if route.startswith(("/v1/files", "/v1/batches")))
    for family, routes in RESOURCE_ROUTES.items():
        for prefix in ("", "/v1"):
            for suffix, methods in routes.items():
                provider.update((method, prefix + family + suffix) for method in methods.split())
    for prefix in ("/models", "/v1beta/models"):
        for operation in ("countTokens", "generateContent", "streamGenerateContent"):
            provider.add(("post", prefix + "/{model_name}:" + operation))
    for prefix in ("/interactions", "/v1beta/interactions"):
        provider |= {
            ("post", prefix), ("get", prefix + "/{interaction_id}"),
            ("delete", prefix + "/{interaction_id}"), ("post", prefix + "/{interaction_id}/cancel"),
        }
    for prefix in ("/realtime", "/v1/realtime", "/openai/v1/realtime"):
        provider |= {("get", prefix), ("post", prefix + "/calls"), ("post", prefix + "/client_secrets")}
    for prefix in ("/search", "/v1/search"):
        provider |= {("post", prefix), ("get", prefix + "/tools"), ("post", prefix + "/{search_tool_name}")}
    for prefix in ("/engines/{model}", "/openai/deployments/{model}"):
        provider.update(("post", prefix + path) for path in ("/chat/completions", "/completions", "/embeddings"))
    provider.update(("post", "/openai/deployments/{model}" + path) for path in ("/images/generations", "/images/edits"))
    provider.update((method, "/openai" + path) for method, path in core | provider if
                    path == "/v1/responses" or path.startswith("/v1/responses/"))
    provider |= {("post", path) for path in ("/v1/rag/query", "/v1/rag/ingest", "/rag/query", "/rag/ingest")}
    return {"core": core, "provider": provider, "management": management}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--show-unlisted", action="store_true", help="List upstream operations absent from the native schema")
    args = parser.parse_args()
    result = subprocess.check_output([
        "docker", "run", "--rm", "--network", "none",
        "-e", "LITELLM_LOCAL_MODEL_COST_MAP=True", "--entrypoint", "python",
        IMAGE, "-c", SCHEMA,
    ], text=True, encoding="utf-8")
    data = json.loads(result.splitlines()[-1])
    upstream = data["schema"]
    paths = {PARAMETER.sub(r"{\1}", path): spec for path, spec in upstream["paths"].items()}
    groups = {}
    for group, selected in operations().items():
        groups[group] = {}
        for method, route in sorted(selected):
            if method in paths.get(route, {}):
                groups[group].setdefault(route, {})[method] = paths[route][method]
        if not groups[group]:
            raise ValueError(f"Missing OpenAPI operations for {group}")

    document = {
        "openapi": upstream["openapi"],
        "info": {"title": "LiteLLM Go Proxy", "version": upstream["info"]["version"]},
        "components": upstream.get("components", {}),
        "groups": groups,
        "catalog": {
            group: [
                {**route, "methods": methods}
                for route in data["catalog"]
                if route["path"] in group_paths and route["methods"]
                if (methods := [method for method in route["methods"] if method.lower() in group_paths[route["path"]]])
            ]
            for group, group_paths in groups.items()
        },
    }
    canonical = json.dumps(document, separators=(",", ":"), sort_keys=True).encode()
    output = ROOT / "cmd/minato-runtime/native-openapi.json.gz"
    output.write_bytes(gzip.compress(canonical, mtime=0))
    print(", ".join(f"{group}={sum(len(ops) for ops in routes.values())}" for group, routes in groups.items()))
    if args.show_unlisted:
        listed = {(route, method) for group in groups.values() for route, methods in group.items() for method in methods}
        http_methods = {"get", "post", "put", "patch", "delete", "head", "options", "trace"}
        upstream_ops = {(route, method) for route, methods in paths.items() for method in http_methods & methods.keys()}
        print(f"{len(listed)} of {len(upstream_ops)} upstream HTTP operations are in the native schema.")
        for route, method in sorted(upstream_ops - listed):
            print(f"{method.upper()} {route}")


if __name__ == "__main__":
    main()
