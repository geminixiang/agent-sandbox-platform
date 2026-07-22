from __future__ import annotations

import argparse
import asyncio
import json
import os
from pathlib import Path
from typing import cast

from agent_sandbox import SandboxClient, StaticToken

from .core import Series, SeriesSpec, build_report, measure_series, render_markdown


async def run(args: argparse.Namespace) -> None:
    metadata = _metadata(args.metadata)
    series: list[Series] = []
    async with SandboxClient(
        base_url=_required("SANDBOX_PLATFORM_URL"),
        credentials=StaticToken(_required("SANDBOX_PLATFORM_TOKEN")),
        timeout=float(args.timeout),
    ) as client:
        for pool in args.pool:
            spec = SeriesSpec(
                scenario="warm-acquire",
                pool=pool,
                cache_state="warm",
                samples=args.samples,
                warmups=args.warmups,
            )

            async def acquire_and_release(selected_pool: str = pool) -> None:
                sandbox = await client.create(pool=selected_pool)
                await sandbox.release()

            series.append(await measure_series(spec, acquire_and_release))

    report = build_report(metadata, series)
    args.json_output.parent.mkdir(parents=True, exist_ok=True)
    args.markdown_output.parent.mkdir(parents=True, exist_ok=True)
    args.json_output.write_text(json.dumps(report.to_dict(), indent=2) + "\n")
    args.markdown_output.write_text(render_markdown(report))


def parser() -> argparse.ArgumentParser:
    value = argparse.ArgumentParser(description="Benchmark Agent Sandbox warm acquisition through the Python SDK")
    value.add_argument("--pool", action="append", required=True, help="logical Pool; repeat for multiple Pools")
    value.add_argument("--samples", type=int, default=10)
    value.add_argument("--warmups", type=int, default=2)
    value.add_argument("--timeout", type=float, default=60)
    value.add_argument("--metadata", type=Path, required=True, help="non-secret environment metadata JSON")
    value.add_argument("--json-output", type=Path, required=True)
    value.add_argument("--markdown-output", type=Path, required=True)
    return value


def _metadata(path: Path) -> dict[str, object]:
    raw: object = json.loads(path.read_text())
    if not isinstance(raw, dict):
        raise ValueError("metadata must be a JSON object")
    raw_mapping = cast(dict[object, object], raw)
    typed: dict[str, object] = {}
    for key, value in raw_mapping.items():
        if not isinstance(key, str):
            raise ValueError("metadata keys must be strings")
        typed[key] = value
    return typed


def _required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


if __name__ == "__main__":
    asyncio.run(run(parser().parse_args()))
