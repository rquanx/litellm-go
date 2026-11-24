"""Generate parameter and provider setting catalogs with the pinned LiteLLM image."""

import gzip
import json
from pathlib import Path
import subprocess

IMAGE = "ghcr.io/berriai/litellm:v1.85.0@sha256:2e8517e2bed423c50ab7e40fb1ac0a9cbe62a764e9d65161d871fb6a9bf75a2d"
OUTPUT = Path(__file__).resolve().parents[1] / "internal/proxyconfig"
EXTRACT = """
import json
import litellm

catalog = {}
identities = {}
for name in sorted(litellm.model_cost):
    if name.startswith("github_copilot/"):
        continue
    try:
        model, provider, _, _ = litellm.get_llm_provider(model=name)
        if not name.startswith(provider + "/"):
            identities[name] = [provider, model]
        params = litellm.get_supported_openai_params(model=model, custom_llm_provider=provider)
    except Exception:
        continue
    if params is None or isinstance(params, list):
        catalog[name] = params
providers = [
    {"name": provider.value, "fields": litellm.get_provider_fields(custom_llm_provider=provider)}
    for provider in litellm.provider_list
]
defaults = {}
for info in providers:
    provider = info["name"]
    if provider in {"github_copilot", "chatgpt"}:
        continue
    try:
        values = []
        for suffix in ("fixture-a", "example-b"):
            model, resolved, _, _ = litellm.get_llm_provider(model=provider + "/" + suffix)
            if resolved != provider:
                raise ValueError("Provider changed")
            values.append(litellm.get_supported_openai_params(model=model, custom_llm_provider=provider))
        if values[0] == values[1] and (values[0] is None or isinstance(values[0], list)):
            defaults[provider] = values[0]
    except Exception:
        continue
print(json.dumps({"params": catalog, "providers": providers, "identities": identities, "defaults": defaults}, separators=(",", ":")))
"""


def main():
    result = subprocess.check_output([
        "docker", "run", "--rm", "--network", "none",
        "-e", "LITELLM_LOCAL_MODEL_COST_MAP=True",
        "--entrypoint", "python", IMAGE, "-c", EXTRACT,
    ], text=True, encoding="utf-8")
    snapshots = json.loads(result.splitlines()[-1])
    catalog = snapshots["params"]
    if len(catalog) < 1000 or "gpt-4" not in catalog or "claude-sonnet-4-5" not in catalog:
        raise ValueError("Incomplete parameter catalog")
    providers = snapshots["providers"]
    if len(providers) < 100 or not any(item["name"] == "openai" for item in providers):
        raise ValueError("Incomplete provider settings catalog")
    for filename, data in [
        ("supported-openai-params.json.gz", catalog),
        ("provider-model-settings.json.gz", providers),
        ("model-identities.json.gz", snapshots["identities"]),
        ("provider-openai-params.json.gz", snapshots["defaults"]),
    ]:
        canonical = json.dumps(data, separators=(",", ":"), sort_keys=True).encode()
        (OUTPUT / filename).write_bytes(gzip.compress(canonical, mtime=0))
    print(f"Wrote {len(catalog)} model entries")


if __name__ == "__main__":
    main()
