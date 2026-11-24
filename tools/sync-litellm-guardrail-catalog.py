"""Generate Dashboard guardrail catalogs from the pinned LiteLLM release."""

import gzip
import json
from pathlib import Path
import runpy
import subprocess

TOOLS = Path(__file__).resolve().parent
IMAGE = runpy.run_path(str(TOOLS / "sync-litellm-openapi.py"))["IMAGE"]
COLLECT = """
import asyncio
import json
from pathlib import Path
from fastapi.encoders import jsonable_encoder
from pydantic_core import PydanticUndefinedType
from litellm.proxy.guardrails import guardrail_endpoints as guardrails

async def collect():
    category_dir = (
        Path(guardrails.__file__).parent /
        "guardrail_hooks/litellm_content_filter/categories"
    )
    categories = {}
    for path in sorted(category_dir.iterdir()):
        if path.suffix in {".yaml", ".json"} and path.stem not in categories:
            categories[path.stem] = await guardrails.get_category_yaml(path.stem)
    return {
        "settings": (await guardrails.get_guardrail_ui_settings()).model_dump(mode="json"),
        "providers": await guardrails.get_provider_specific_params(),
        "airlines": await guardrails.get_major_airlines(),
        "categories": categories,
    }

data = jsonable_encoder(asyncio.run(collect()), custom_encoder={PydanticUndefinedType: lambda _: None})
print(json.dumps(data, separators=(",", ":"), sort_keys=True))
"""

if __name__ == "__main__":
    result = subprocess.check_output([
        "docker", "run", "--rm", "--network", "none",
        "-e", "LITELLM_LOCAL_MODEL_COST_MAP=True",
        "--entrypoint", "python", IMAGE, "-c", COLLECT,
    ], text=True, encoding="utf-8")
    data = json.loads(result.splitlines()[-1])
    canonical = json.dumps(data, separators=(",", ":"), sort_keys=True).encode()
    output = TOOLS.parent / "internal/proxyconfig/guardrail-catalog.json.gz"
    output.write_bytes(gzip.compress(canonical, mtime=0))
