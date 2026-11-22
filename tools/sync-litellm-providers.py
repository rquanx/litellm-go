"""Generate pinned provider, pricing, and endpoint metadata without importing LiteLLM."""

import argparse
import ast
import gzip
import io
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
TARGET = ROOT / "internal/proxyconfig/openai-compatible.json"
SETTINGS = ROOT / "internal/proxyconfig/openai-provider-settings.json"
PRICES = ROOT / "internal/proxyconfig/model-token-prices.json"
CACHE_PRICES = ROOT / "internal/proxyconfig/model-cache-prices.json"


def compressed_json(data):
    output = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as stream:
        stream.write(json.dumps(data, separators=(",", ":")).encode())
    return output.getvalue()


def endpoint_catalog(source):
    tree = ast.parse(source("litellm/proxy/public_endpoints/public_endpoints.py"))
    metadata = next(
        ast.literal_eval(node.value) for node in tree.body
        if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name)
        and node.target.id == "_ENDPOINT_METADATA"
    )
    providers = json.loads(source("litellm/provider_endpoints_support_backup.json"))["providers"]
    keys = dict.fromkeys(key for provider in providers.values() for key in provider.get("endpoints", {}))
    endpoints = []
    for key in keys:
        endpoint = metadata.get(key, {
            "label": key.replace("_", " ").title(),
            "endpoint": "/" + key.replace("_", "/"),
        })
        supporting = [
            {"slug": slug, "display_name": re.sub(r"\s*\(`[^`]+`\)\s*$", "", provider.get("display_name", slug)).strip()}
            for slug, provider in providers.items()
            if provider.get("endpoints", {}).get(key)
        ]
        endpoints.append({"key": key, **endpoint, "providers": supporting})
    return {"endpoints": endpoints}


def agent_fields_catalog(source, providers):
    schema = ast.parse(source("litellm/types/proxy/public_endpoints/public_endpoints.py"))
    models = {
        node.name: {
            field.target.id: field.value for field in node.body
            if isinstance(field, ast.AnnAssign) and isinstance(field.target, ast.Name)
        }
        for node in schema.body if isinstance(node, ast.ClassDef)
    }

    def with_defaults(model, data):
        return {
            key: data[key] if key in data else ast.literal_eval(default)
            for key, default in models[model].items()
        }

    agents = json.loads(source("litellm/proxy/public_endpoints/agent_create_fields.json"))
    provider_fields = {provider["provider"]: provider["credential_fields"] for provider in providers}
    for agent in agents:
        inherited_provider = agent.pop("inherit_credentials_from_provider", None)
        agent["credential_fields"].extend(
            {**field, "include_in_litellm_params": True}
            for field in provider_fields.get(inherited_provider, [])
        )
        agent["credential_fields"] = [
            with_defaults("AgentCredentialField", field) for field in agent["credential_fields"]
        ]
    return [with_defaults("AgentCreateInfo", agent) for agent in agents]


def public_catalogs(checkout, revision, prices):
    def source(path):
        return subprocess.check_output(["git", "-C", checkout, "show", f"{revision}:{path}"])

    tree = ast.parse(source("litellm/types/utils.py"))
    provider_enum = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "LlmProviders")
    providers = sorted({
        field.value.value for field in provider_enum.body
        if isinstance(field, ast.Assign) and isinstance(field.value, ast.Constant) and isinstance(field.value.value, str)
    })
    fields = json.loads(source("litellm/proxy/public_endpoints/provider_create_fields.json"))
    blog_posts = json.loads(source("litellm/blog_posts.json")).get("posts", [])[:5]
    return {
        "public-model-cost-map.json.gz": compressed_json(prices),
        "public-providers.json.gz": compressed_json(providers),
        "public-provider-fields.json.gz": compressed_json(fields),
        "public-endpoints.json.gz": compressed_json(endpoint_catalog(source)),
        "public-agent-fields.json.gz": compressed_json(agent_fields_catalog(source, fields)),
        "public-blog-posts.json.gz": compressed_json({"posts": blog_posts}),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkout", help="Path to the LiteLLM Git checkout")
    parser.add_argument("--check", action="store_true", help="Verify the committed provider list")
    args = parser.parse_args()
    revision = (ROOT / "upstream/litellm.ref").read_text().strip()
    constants = subprocess.check_output(
        ["git", "-C", args.checkout, "show", f"{revision}:litellm/constants.py"]
    )
    for node in ast.parse(constants).body:
        if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.target.id == "openai_compatible_providers":
            providers = ast.literal_eval(node.value)
            break
    else:
        raise SystemExit("Upstream provider list was not found")
    output = json.dumps(sorted(set(providers)), indent=2) + "\n"
    settings = json.loads(subprocess.check_output(
        ["git", "-C", args.checkout, "show", f"{revision}:litellm/llms/openai_like/providers.json"]
    ))
    settings_output = json.dumps(settings, sort_keys=True, indent=2) + "\n"
    upstream_prices = json.loads(subprocess.check_output(
        ["git", "-C", args.checkout, "show", f"{revision}:model_prices_and_context_window.json"]
    ))
    price_fields = ("input_cost_per_token", "output_cost_per_token")
    prices = {}
    for model, metadata in upstream_prices.items():
        if model == "sample_spec":
            continue
        entry = [metadata.get(field) for field in price_fields]
        if any(type(price) in (int, float) for price in entry):
            prices[model] = [price if type(price) in (int, float) else None for price in entry]
    price_output = "{\n" + ",\n".join(
        f"  {json.dumps(model)}: {json.dumps(prices[model])}" for model in sorted(prices)
    ) + "\n}\n"
    cache_prices = {}
    for model, metadata in upstream_prices.items():
        rates = [metadata.get(field) for field in ("cache_read_input_token_cost", "cache_creation_input_token_cost")]
        if model != "sample_spec" and any(type(rate) in (int, float) for rate in rates):
            cache_prices[model] = [rate if type(rate) in (int, float) else None for rate in rates]
    cache_output = json.dumps(cache_prices, sort_keys=True, indent=2) + "\n"
    public = public_catalogs(args.checkout, revision, upstream_prices)
    if args.check:
        if TARGET.read_text(encoding="utf-8") != output or SETTINGS.read_text(encoding="utf-8") != settings_output:
            raise SystemExit("Go provider list differs from the pinned LiteLLM source")
        print(f"{len(set(providers))} provider names match LiteLLM {revision}")
        print(f"{len(settings)} provider settings match LiteLLM {revision}")
        if PRICES.read_text(encoding="utf-8") != price_output:
            raise SystemExit("Go token prices differ from the pinned LiteLLM source")
        print(f"{len(prices)} model prices match LiteLLM {revision}")
        if CACHE_PRICES.read_text(encoding="utf-8") != cache_output:
            raise SystemExit("Go cache prices differ from the pinned LiteLLM source")
        print(f"{len(cache_prices)} cache prices match LiteLLM {revision}")
        for name, data in public.items():
            if gzip.decompress((TARGET.parent / name).read_bytes()) != gzip.decompress(data):
                raise SystemExit(f"{name} differs from the pinned LiteLLM source")
        print(f"{len(public)} public catalogs match LiteLLM {revision}")
    else:
        TARGET.write_text(output, encoding="utf-8")
        SETTINGS.write_text(settings_output, encoding="utf-8")
        PRICES.write_text(price_output, encoding="utf-8")
        CACHE_PRICES.write_text(cache_output, encoding="utf-8")
        for name, data in public.items():
            (TARGET.parent / name).write_bytes(data)


if __name__ == "__main__":
    main()
