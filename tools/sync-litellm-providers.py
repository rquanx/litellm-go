"""Read the upstream OpenAI-compatible provider list without importing LiteLLM."""

import argparse
import ast
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
TARGET = ROOT / "internal/proxyconfig/openai-compatible.json"
SETTINGS = ROOT / "internal/proxyconfig/openai-provider-settings.json"


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
    if args.check:
        if TARGET.read_text(encoding="utf-8") != output or SETTINGS.read_text(encoding="utf-8") != settings_output:
            raise SystemExit("Go provider list differs from the pinned LiteLLM source")
        print(f"{len(set(providers))} provider names match LiteLLM {revision}")
        print(f"{len(settings)} provider settings match LiteLLM {revision}")
    else:
        TARGET.write_text(output, encoding="utf-8")
        SETTINGS.write_text(settings_output, encoding="utf-8")


if __name__ == "__main__":
    main()
