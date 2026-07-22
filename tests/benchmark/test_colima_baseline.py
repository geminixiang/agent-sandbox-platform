from __future__ import annotations

import hashlib
from collections.abc import AsyncIterator
from typing import cast

import pytest
from agent_sandbox import FileDownload

from .colima_baseline import (
    STREAM_CHUNK_BYTES,
    Integrity,
    consume_download,
    measure_acquire_series,
    stream_chunks,
    stream_specs,
)
from .core import SeriesSpec, measure_series


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


@pytest.mark.asyncio
async def test_stream_chunks_are_lazy_and_bounded() -> None:
    integrity = Integrity()
    source = stream_chunks(STREAM_CHUNK_BYTES * 2, integrity)

    assert integrity.count == 0
    first = await anext(source)
    assert len(first) == STREAM_CHUNK_BYTES
    assert integrity.count == STREAM_CHUNK_BYTES
    await source.aclose()
    assert integrity.count == STREAM_CHUNK_BYTES


class FakeDownload:
    def __init__(self, chunks: list[bytes]) -> None:
        self.chunks = chunks
        self.consumed = 0

    def __aiter__(self) -> AsyncIterator[bytes]:
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[bytes]:
        for chunk in self.chunks:
            self.consumed += 1
            yield chunk


@pytest.mark.asyncio
async def test_stream_read_consumes_every_chunk_and_checks_integrity() -> None:
    chunks = [b"one", b"two", b"three"]
    download = FakeDownload(chunks)
    content = b"".join(chunks)

    await consume_download(
        cast(FileDownload, download),
        len(content),
        hashlib.sha256(content).hexdigest(),
    )

    assert download.consumed == len(chunks)


@pytest.mark.asyncio
async def test_failed_stream_integrity_is_a_failed_sample() -> None:
    async def operation() -> None:
        download = FakeDownload([b"content"])
        await consume_download(cast(FileDownload, download), 7, hashlib.sha256(b"wrong").hexdigest())

    series = await measure_series(SeriesSpec("stream-read-test", "coding", "warm", 1, 0), operation)

    assert series.summary.failures == 1
    assert series.samples[0].error_type == "ValueError"


def test_stream_series_have_stable_distinct_identities() -> None:
    specs = stream_specs(3)

    assert [(spec.scenario, spec.pool, spec.cache_state) for spec in specs] == [
        ("stream-write-1MiB", "coding", "warm"),
        ("stream-read-1MiB", "coding", "warm"),
        ("stream-write-10MiB", "coding", "warm"),
        ("stream-read-10MiB", "coding", "warm"),
        ("stream-write-32MiB", "coding", "warm"),
        ("stream-read-32MiB", "coding", "warm"),
    ]
    assert all(spec.samples == 3 and spec.warmups == 1 for spec in specs)
