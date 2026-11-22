"""Read the upstream OpenAI-compatible provider list without importing LiteLLM."""

import argparse
import ast
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
TARGET = ROOT / "internal/proxyconfig/openai-compatible.json"
SETTINGS = ROOT / "internal/proxyconfig/openai-provider-settings.json"
PRICES = ROOT / "internal/proxyconfig/model-token-prices.json"
CACHE_PRICES = ROOT / "internal/proxyconfig/model-cache-prices.json"


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
        price = metadata.get("cache_read_input_token_cost")
        if model != "sample_spec" and type(price) in (int, float):
            cache_prices[model] = price
    cache_output = json.dumps(cache_prices, sort_keys=True, indent=2) + "\n"
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
    else:
        TARGET.write_text(output, encoding="utf-8")
        SETTINGS.write_text(settings_output, encoding="utf-8")
        PRICES.write_text(price_output, encoding="utf-8")
        CACHE_PRICES.write_text(cache_output, encoding="utf-8")


if __name__ == "__main__":
    main()
