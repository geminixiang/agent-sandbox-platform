from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import os
import time
from collections.abc import AsyncGenerator, Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol, cast

from agent_sandbox import FileDownload, Sandbox, SandboxClient

from .core import Sample, Series, SeriesSpec, build_report, build_series, measure_series, render_markdown

AcquireOperation = Callable[[str], Awaitable[Sandbox]]
ReadyOperation = Callable[[str], Awaitable[None]]
ReleaseOperation = Callable[[Sandbox], Awaitable[object]]
STREAM_CHUNK_BYTES = 64 * 1024
STREAM_SIZES = (1 * 1024 * 1024, 10 * 1024 * 1024, 32 * 1024 * 1024)
_PAYLOAD_BLOCK = hashlib.sha256(b"agent-sandbox-benchmark").digest()


class Digest(Protocol):
    def update(self, value: bytes, /) -> None: ...
    def hexdigest(self) -> str: ...


@dataclass(slots=True)
class Integrity:
    count: int = 0
    digest: Digest = field(default_factory=hashlib.sha256)

    def update(self, chunk: bytes) -> None:
        self.count += len(chunk)
        self.digest.update(chunk)

    def hexdigest(self) -> str:
        return self.digest.hexdigest()


async def stream_chunks(size: int, integrity: Integrity) -> AsyncGenerator[bytes, None]:
    remaining = size
    while remaining:
        length = min(STREAM_CHUNK_BYTES, remaining)
        chunk = (_PAYLOAD_BLOCK * ((length + len(_PAYLOAD_BLOCK) - 1) // len(_PAYLOAD_BLOCK)))[:length]
        integrity.update(chunk)
        yield chunk
        remaining -= length


def stream_digest(size: int) -> str:
    integrity = Integrity()
    remaining = size
    while remaining:
        length = min(STREAM_CHUNK_BYTES, remaining)
        integrity.update((_PAYLOAD_BLOCK * ((length + len(_PAYLOAD_BLOCK) - 1) // len(_PAYLOAD_BLOCK)))[:length])
        remaining -= length
    return integrity.hexdigest()


def stream_specs(samples: int) -> tuple[SeriesSpec, ...]:
    return tuple(
        SeriesSpec(f"stream-{operation}-{size // (1024 * 1024)}MiB", "coding", "warm", samples, 1)
        for size in STREAM_SIZES
        for operation in ("write", "read")
    )


def verify_integrity(integrity: Integrity, size: int, digest: str) -> None:
    if integrity.count != size or integrity.hexdigest() != digest:
        raise ValueError("streamed content failed benchmark integrity verification")


async def consume_download(download: FileDownload, size: int, digest: str) -> None:
    integrity = Integrity()
    pending = bytearray()
    async for received in download:
        pending.extend(received)
        while len(pending) >= STREAM_CHUNK_BYTES:
            integrity.update(bytes(pending[:STREAM_CHUNK_BYTES]))
            del pending[:STREAM_CHUNK_BYTES]
    if pending:
        integrity.update(bytes(pending))
    verify_integrity(integrity, size, digest)


async def measure_acquire_series(
    spec: SeriesSpec,
    *,
    concurrency: int,
    acquire: AcquireOperation,
    wait_ready: ReadyOperation,
    release: ReleaseOperation,
    clock_ns: Callable[[], int] = time.perf_counter_ns,
) -> Series:
    samples: list[Sample] = []
    for sequence in range(spec.warmups + spec.samples):
        await wait_ready(spec.pool)
        started = clock_ns()
        sandboxes: list[Sandbox] = []
        try:
            sandboxes = list(await asyncio.gather(*(acquire(spec.pool) for _ in range(concurrency))))
        except Exception as error:
            sample = _failed(started, error, clock_ns=clock_ns)
        else:
            sample = _successful(started, clock_ns=clock_ns)
        finally:
            if sandboxes:
                await asyncio.gather(*(release(sandbox) for sandbox in sandboxes))
            await wait_ready(spec.pool)
        if sequence >= spec.warmups:
            samples.append(sample)
    return build_series(spec, samples)


class Benchmark:
    def __init__(
        self,
        client: SandboxClient,
        observer_url: str,
        samples: int,
        warmups: int,
        concurrency_samples: int,
        stream_samples: int = 3,
    ) -> None:
        self.client = client
        self.observer_url = observer_url.rstrip("/")
        self.samples = samples
        self.warmups = warmups
        self.concurrency_samples = concurrency_samples
        self.stream_samples = stream_samples

    async def run(self) -> list[Series]:
        series: list[Series] = []
        for pool in ("coding", "browser"):
            series.append(await self._warm_acquire(pool))
            series.append(await self._replenishment(pool))
            for concurrency in (1, 2, 4):
                series.append(await self._concurrent_acquire(pool, concurrency))
        series.extend(await self._coding_operations())
        series.extend(await self._streaming_operations())
        series.extend(await self._browser_operations())
        return series

    async def _warm_acquire(self, pool: str) -> Series:
        spec = SeriesSpec("warm-acquire", pool, "warm", self.samples, self.warmups)
        return await measure_acquire_series(
            spec,
            concurrency=1,
            acquire=lambda selected_pool: self.client.create(pool=selected_pool),
            wait_ready=self._wait_ready,
            release=lambda sandbox: sandbox.release(),
        )

    async def _concurrent_acquire(self, pool: str, concurrency: int) -> Series:
        spec = SeriesSpec(
            f"warm-start-acquire-concurrency-{concurrency}",
            pool,
            "warm",
            self.concurrency_samples,
            min(self.warmups, 1),
        )
        return await measure_acquire_series(
            spec,
            concurrency=concurrency,
            acquire=lambda selected_pool: self.client.create(pool=selected_pool),
            wait_ready=self._wait_ready,
            release=lambda sandbox: sandbox.release(),
        )

    async def _replenishment(self, pool: str) -> Series:
        spec = SeriesSpec("warmpool-replenishment", pool, "warm", self.samples, 0)
        samples: list[Sample] = []
        for _ in range(self.samples):
            await self._wait_ready(pool)
            sandbox = await self.client.create(pool=pool)
            started = time.perf_counter_ns()
            await sandbox.release()
            try:
                await self._wait_ready(pool)
            except Exception as error:
                samples.append(_failed(started, error))
            else:
                samples.append(_successful(started))
        return build_series(spec, samples)

    async def _coding_operations(self) -> list[Series]:
        sandbox = await self.client.create(pool="coding")
        try:
            result = [
                await self._measure_command(sandbox, "exec-small", "printf ok"),
                await self._measure_command(sandbox, "exec-stdout-1-kib", "head -c 1024 /dev/zero | tr '\\0' x"),
                await self._measure_command(sandbox, "exec-stdout-1-mib", "head -c 1048576 /dev/zero | tr '\\0' x"),
            ]
            for size in (1024, 512 * 1024):
                result.append(await self._measure_file_write(sandbox, size))
            for size in (1024, 1024 * 1024, 7 * 1024 * 1024):
                result.append(await self._measure_file_read(sandbox, size))
            return result
        finally:
            await sandbox.release()
            await self._wait_ready("coding")

    async def _measure_command(self, sandbox: Sandbox, scenario: str, command: str) -> Series:
        spec = SeriesSpec(scenario, "coding", "warm", self.samples, self.warmups)

        async def operation() -> None:
            await sandbox.run(command, check=True)

        return await measure_series(spec, operation)

    async def _measure_file_write(self, sandbox: Sandbox, size: int) -> Series:
        payload = _payload(size)
        spec = SeriesSpec(f"file-write-{size}-bytes", "coding", "warm", self.samples, self.warmups)
        path = f"/workspace/write-{size}.bin"

        async def operation() -> None:
            await sandbox.files.write_bytes(path, payload)

        return await measure_series(spec, operation)

    async def _measure_file_read(self, sandbox: Sandbox, size: int) -> Series:
        payload = _payload(size)
        expected = hashlib.sha256(payload).digest()
        spec = SeriesSpec(f"file-read-{size}-bytes", "coding", "warm", self.samples, self.warmups)
        path = f"/workspace/read-{size}.bin"
        block = repr(payload[:32])
        await sandbox.run(f"python -c \"open('{path}', 'wb').write(({block}) * {size // 32})\"", check=True)

        async def operation() -> None:
            received = await sandbox.files.read_bytes(path)
            if hashlib.sha256(received).digest() != expected:
                raise ValueError("file checksum mismatch")

        return await measure_series(spec, operation)

    async def _streaming_operations(self) -> list[Series]:
        sandbox = await self.client.create(pool="coding")
        results: list[Series] = []
        specifications = stream_specs(self.stream_samples)
        try:
            for index, size in enumerate(STREAM_SIZES):
                write_spec = specifications[index * 2]
                read_spec = specifications[index * 2 + 1]
                size_mib = size // (1024 * 1024)
                digest = stream_digest(size)
                path = f"/workspace/benchmark-stream-{size_mib}MiB.bin"

                async def write() -> None:
                    integrity = Integrity()
                    await sandbox.files.write_stream(
                        path,
                        stream_chunks(size, integrity),
                        size_bytes=size,
                        sha256=digest,
                    )
                    verify_integrity(integrity, size, digest)

                results.append(
                    await measure_series(write_spec, write)
                )

                # Prepare a known fixture outside the read timing, even though the
                # preceding write series leaves the same deterministic content.
                fixture_integrity = Integrity()
                await sandbox.files.write_stream(
                    path,
                    stream_chunks(size, fixture_integrity),
                    size_bytes=size,
                    sha256=digest,
                )
                verify_integrity(fixture_integrity, size, digest)

                async def read() -> None:
                    async with sandbox.files.read_stream(path) as download:
                        if (download.size_bytes, download.sha256) != (size, digest):
                            raise ValueError("download metadata failed benchmark integrity verification")
                        await consume_download(download, size, digest)

                results.append(
                    await measure_series(read_spec, read)
                )
            return results
        finally:
            await sandbox.release()
            await self._wait_ready("coding")

    async def _browser_operations(self) -> list[Series]:
        sandbox = await self.client.create(pool="browser")
        source = '''import { chromium } from "playwright-core";
const started = performance.now();
const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true });
const launched = performance.now();
const page = await browser.newPage();
const pageCreated = performance.now();
await page.setContent("<main><h1>Benchmark ready</h1></main>", { waitUntil: "domcontentloaded" });
await page.locator("h1").waitFor();
const domReady = performance.now();
await page.screenshot({ path: "/workspace/benchmark.png" });
const screenshot = performance.now();
await browser.close();
console.log(JSON.stringify({ launchMs: launched-started, pageMs: pageCreated-launched, domMs: domReady-pageCreated, screenshotMs: screenshot-domReady }));
'''
        await sandbox.files.write_text("/workspace/benchmark.mjs", source)
        try:
            names = ("chromium-launch", "browser-new-page", "browser-dom-ready", "browser-screenshot")
            milestones: dict[str, list[Sample]] = {name: [] for name in names}
            total_runs = self.warmups + self.samples
            for sequence in range(total_runs):
                try:
                    result = await sandbox.run(
                        "test -e node_modules || ln -s /opt/browser/node_modules node_modules; node benchmark.mjs",
                        cwd="/workspace",
                        check=True,
                    )
                    raw_values: object = json.loads(result.stdout)
                    if not isinstance(raw_values, dict):
                        raise ValueError("browser milestone output is not an object")
                    values = cast(dict[object, object], raw_values)
                    current = {
                        "chromium-launch": _number(values, "launchMs"),
                        "browser-new-page": _number(values, "pageMs"),
                        "browser-dom-ready": _number(values, "domMs"),
                        "browser-screenshot": _number(values, "screenshotMs"),
                    }
                except Exception as error:
                    if sequence >= self.warmups:
                        for name in names:
                            milestones[name].append(Sample(0, False, type(error).__name__))
                else:
                    if sequence >= self.warmups:
                        for name in names:
                            milestones[name].append(Sample(round(current[name], 3), True))
            return [
                build_series(SeriesSpec(name, "browser", "warm", self.samples, self.warmups), milestones[name])
                for name in names
            ]
        finally:
            await sandbox.release()
            await self._wait_ready("browser")

    async def _wait_ready(self, pool: str) -> None:
        await _request_json(f"{self.observer_url}/ready/{pool}")


async def main() -> None:
    metadata_path = Path(_required("SANDBOX_BENCHMARK_METADATA"))
    output_json = Path(_required("SANDBOX_BENCHMARK_JSON"))
    output_markdown = Path(_required("SANDBOX_BENCHMARK_MARKDOWN"))
    metadata_value: object = json.loads(metadata_path.read_text())
    if not isinstance(metadata_value, dict):
        raise ValueError("benchmark metadata must be an object")
    raw_metadata = cast(dict[object, object], metadata_value)
    metadata: dict[str, object] = {}
    for key, value in raw_metadata.items():
        if not isinstance(key, str):
            raise ValueError("benchmark metadata keys must be strings")
        metadata[key] = value
    samples = int(os.environ.get("SANDBOX_BENCHMARK_SAMPLES", "10"))
    warmups = int(os.environ.get("SANDBOX_BENCHMARK_WARMUPS", "2"))
    concurrency_samples = int(os.environ.get("SANDBOX_BENCHMARK_CONCURRENCY_SAMPLES", "3"))
    stream_samples = int(os.environ.get("SANDBOX_BENCHMARK_STREAM_SAMPLES", "3"))
    metadata["streaming"] = {
        "chunkBytes": STREAM_CHUNK_BYTES,
        "sizesMiB": [size // (1024 * 1024) for size in STREAM_SIZES],
        "samples": stream_samples,
        "warmups": 1,
        "fixturePreparationTimed": False,
        "integrity": "incremental-sha256-and-byte-count",
    }
    async with SandboxClient(
        base_url=_required("SANDBOX_PLATFORM_URL"),
        credentials=_subject_token,
        timeout=180,
    ) as client:
        benchmark = Benchmark(
            client,
            _required("SANDBOX_BENCHMARK_OBSERVER_URL"),
            samples,
            warmups,
            concurrency_samples,
            stream_samples,
        )
        report = build_report(metadata, await benchmark.run())
    output_json.write_text(json.dumps(report.to_dict(), indent=2) + "\n")
    output_markdown.write_text(render_markdown(report))


async def _request_json(url: str) -> object:
    import urllib.request

    def request() -> object:
        with urllib.request.urlopen(url, timeout=180) as response:
            return json.loads(response.read())

    return await asyncio.to_thread(request)


def _subject_token() -> str:
    claims = json.dumps(
        {
            "consumerId": _required("SANDBOX_TEST_CONSUMER_ID"),
            "subjectId": "benchmark",
            "exp": int(time.time()) + 300,
        },
        separators=(",", ":"),
    ).encode()
    payload = base64.urlsafe_b64encode(claims).decode().rstrip("=")
    signed = f"v1.{payload}"
    signature = base64.urlsafe_b64encode(
        hmac.new(
            _required("SANDBOX_TEST_CONSUMER_SECRET").encode(),
            signed.encode(),
            hashlib.sha256,
        ).digest()
    ).decode().rstrip("=")
    return f"{signed}.{signature}"


def _number(value: dict[object, object], key: str) -> float:
    item = value.get(key)
    if not isinstance(item, int | float):
        raise ValueError(f"browser milestone {key} is not numeric")
    return float(item)


def _payload(size: int) -> bytes:
    return (_PAYLOAD_BLOCK * ((size + len(_PAYLOAD_BLOCK) - 1) // len(_PAYLOAD_BLOCK)))[:size]


def _successful(started: int, *, clock_ns: Callable[[], int] = time.perf_counter_ns) -> Sample:
    return Sample(round((clock_ns() - started) / 1_000_000, 3), True)


def _failed(started: int, error: Exception, *, clock_ns: Callable[[], int] = time.perf_counter_ns) -> Sample:
    return Sample(round((clock_ns() - started) / 1_000_000, 3), False, type(error).__name__)


def _required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


if __name__ == "__main__":
    asyncio.run(main())
