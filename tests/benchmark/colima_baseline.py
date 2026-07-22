from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import os
import time
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import cast

from agent_sandbox import Sandbox, SandboxClient

from .core import Sample, Series, SeriesSpec, build_report, build_series, measure_series, render_markdown

AcquireOperation = Callable[[str], Awaitable[Sandbox]]
ReadyOperation = Callable[[str], Awaitable[None]]
ReleaseOperation = Callable[[Sandbox], Awaitable[object]]


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
    def __init__(self, client: SandboxClient, observer_url: str, samples: int, warmups: int) -> None:
        self.client = client
        self.observer_url = observer_url.rstrip("/")
        self.samples = samples
        self.warmups = warmups

    async def run(self) -> list[Series]:
        series: list[Series] = []
        for pool in ("coding", "browser"):
            series.append(await self._warm_acquire(pool))
            series.append(await self._replenishment(pool))
            for concurrency in (1, 2, 4):
                series.append(await self._concurrent_acquire(pool, concurrency))
        series.extend(await self._coding_operations())
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
            self.samples,
            self.warmups,
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
    async with SandboxClient(
        base_url=_required("SANDBOX_PLATFORM_URL"),
        credentials=_subject_token,
        timeout=180,
    ) as client:
        benchmark = Benchmark(client, _required("SANDBOX_BENCHMARK_OBSERVER_URL"), samples, warmups)
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
    block = hashlib.sha256(b"agent-sandbox-benchmark").digest()
    return (block * ((size + len(block) - 1) // len(block)))[:size]


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
