"""Inventory declared LiteLLM Proxy routes and provider packages at a Git revision."""

import argparse
import ast
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
HTTP_METHODS = {"delete", "get", "head", "options", "patch", "post", "put", "trace"}


def git(checkout, *args):
    return subprocess.check_output(["git", "-C", checkout, *args])


def blobs(checkout, revision, paths):
    objects = [f"{revision}:{path}" for path in paths]
    data = subprocess.check_output(
        ["git", "-C", checkout, "cat-file", "--batch"],
        input=("\n".join(objects) + "\n").encode(),
    )
    offset = 0
    for path in paths:
        end = data.index(b"\n", offset)
        header = data[offset:end].split()
        if len(header) != 3 or header[1] != b"blob":
            raise ValueError(f"Could not read upstream blob {path}")
        size = int(header[2])
        offset = end + 1
        yield path, data[offset:offset + size]
        offset += size + 1


def declared_routes(source):
    tree = ast.parse(source)
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        for decorator in node.decorator_list:
            if not isinstance(decorator, ast.Call) or not isinstance(decorator.func, ast.Attribute):
                continue
            method = decorator.func.attr
            if method not in HTTP_METHODS or not decorator.args:
                continue
            path = decorator.args[0]
            if isinstance(path, ast.Constant) and isinstance(path.value, str) and path.value.startswith("/"):
                yield method.upper(), path.value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkout", help="Path to the LiteLLM Git checkout")
    parser.add_argument("revision", nargs="?", help="Upstream tag or commit")
    args = parser.parse_args()
    revision = args.revision or (ROOT / "upstream/litellm.ref").read_text().strip()
    revision = git(args.checkout, "rev-parse", "--verify", "--end-of-options", f"{revision}^{{commit}}").decode().strip()
    sources = git(args.checkout, "ls-tree", "-r", "--name-only", revision, "litellm/proxy").decode().splitlines()
    paths = [name for name in sources if name.endswith(".py")]
    declarations = {}
    for path, source in blobs(args.checkout, revision, paths):
        for method, route in declared_routes(source):
            declarations.setdefault(f"{method} {route}", set()).add(path)
    providers = git(args.checkout, "ls-tree", "-d", "--name-only", f"{revision}:litellm/llms").decode().splitlines()
    print(json.dumps({
        "revision": revision,
        "provider_and_shared_packages": providers,
        "proxy_declarations": {route: sorted(declarations[route]) for route in sorted(declarations)},
    }, indent=2))


if __name__ == "__main__":
    main()
