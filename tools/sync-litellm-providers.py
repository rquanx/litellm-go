"""Read the upstream OpenAI-compatible provider list without importing LiteLLM."""

import argparse
import ast
import gzip
import io
import json
from pathlib import Path
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
    return {
        "public-model-cost-map.json.gz": compressed_json(prices),
        "public-providers.json.gz": compressed_json(providers),
        "public-provider-fields.json.gz": compressed_json(fields),
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
