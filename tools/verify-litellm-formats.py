"""Compare the Go format fixtures with the pinned LiteLLM Python transforms."""

import argparse
import ast
import json
from pathlib import Path
import subprocess
from typing import Any, Dict, Optional, Union

ROOT = Path(__file__).resolve().parents[1]
TRANSFORMS = {
    "responses_to_chat": (
        "litellm/responses/litellm_completion_transformation/transformation.py",
        "_transform_text_format_to_response_format",
    ),
    "chat_to_responses": (
        "litellm/completion_extras/litellm_responses_transformation/transformation.py",
        "_transform_response_format_to_text_format",
    ),
}


def load_transform(checkout, revision, path, name):
    source = subprocess.check_output(
        ["git", "-C", checkout, "show", f"{revision}:{path}"]
    )
    tree = ast.parse(source, filename=path)
    function = next(
        node for node in ast.walk(tree)
        if isinstance(node, ast.FunctionDef) and node.name == name
    )
    # These helpers are self-contained. Preserve the method signature for the instance helper.
    function.decorator_list = []
    namespace = {"Any": Any, "Dict": Dict, "Optional": Optional, "Union": Union}
    exec(compile(ast.Module(body=[function], type_ignores=[]), path, "exec"), namespace)
    transform = namespace[name]
    if function.args.args[0].arg == "self":
        return transform.__get__(object())
    return transform


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkout", help="Path to the LiteLLM checkout")
    parser.add_argument("revision", nargs="?", help="Upstream tag or commit to compare")
    args = parser.parse_args()
    revision = args.revision or (ROOT / "upstream/litellm.ref").read_text().strip()
    revision = subprocess.check_output(
        ["git", "-C", args.checkout, "rev-parse", "--verify", "--end-of-options", f"{revision}^{{commit}}"],
        text=True,
    ).strip()
    transforms = {
        direction: load_transform(args.checkout, revision, *source)
        for direction, source in TRANSFORMS.items()
    }
    fixture = ROOT / "internal/runtime/testdata/litellm-formats.json"
    cases = json.loads(fixture.read_text(encoding="utf-8"))
    if not cases:
        raise SystemExit("Missing parity cases")
    mismatches = []
    for case in cases:
        result = transforms[case["direction"]](case["input"])
        if result != case["output"]:
            mismatches.append(f"{case['direction']}/{case['name']}: upstream returned {result!r}")
    if mismatches:
        raise SystemExit("\n".join(mismatches))
    print(f"{len(cases)} format cases match LiteLLM {revision}")


if __name__ == "__main__":
    main()
