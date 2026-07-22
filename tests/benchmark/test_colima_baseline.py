from __future__ import annotations

import pytest

from .colima_baseline import measure_acquire_series
from .core import SeriesSpec


class FakeSandbox:
    pass


@pytest.mark.asyncio
async def test_acquire_timing_excludes_release_and_replenishment() -> None:
    events: list[str] = []
    ticks = iter(
        [
            0, 5_000_000,  # warmup acquire timing
            100_000_000, 107_000_000,  # recorded acquire timing
        ]
    )

    async def ready(pool: str) -> None:
        events.append(f"ready:{pool}")

    async def acquire(pool: str) -> FakeSandbox:
        events.append(f"acquire:{pool}")
        return FakeSandbox()

    async def release(_sandbox: object) -> None:
        events.append("release")

    series = await measure_acquire_series(
        SeriesSpec("warm-acquire", "coding", "warm", samples=1, warmups=1),
        concurrency=1,
        acquire=acquire,  # type: ignore[arg-type]
        wait_ready=ready,
        release=release,  # type: ignore[arg-type]
        clock_ns=lambda: next(ticks),
    )

    assert series.samples[0].duration_ms == 7.0
    assert events == [
        "ready:coding",
        "acquire:coding",
        "release",
        "ready:coding",
        "ready:coding",
        "acquire:coding",
        "release",
        "ready:coding",
    ]
