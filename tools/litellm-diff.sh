#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'Usage: bash tools/litellm-diff.sh <LiteLLM checkout> <target tag or commit>\n' >&2
  exit 2
fi

runtime_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
baseline="$(tr -d '\r\n' < "$runtime_root/upstream/litellm.ref")"
reference="$1"
base_commit="$(git -C "$reference" rev-parse --verify --end-of-options "${baseline}^{commit}")"
target_commit="$(git -C "$reference" rev-parse --verify --end-of-options "${2}^{commit}")"

printf 'LiteLLM %s -> %s\n' "$base_commit" "$target_commit"
git -C "$reference" --no-pager diff --name-status "$base_commit" "$target_commit" -- \
  litellm tests pyproject.toml model_prices_and_context_window.json provider_endpoints_support.json
