from __future__ import annotations

import math
import time
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from typing import Literal

CacheState = Literal["warm", "image-cached", "image-uncached", "new-node"]


@dataclass(frozen=True, slots=True)
class SeriesSpec:
    scenario: str
    pool: str
    cache_state: CacheState
    samples: int
    warmups: int = 1
    unit: str = "milliseconds"

    def __post_init__(self) -> None:
        if not self.scenario or not self.pool:
            raise ValueError("scenario and pool are required")
        if self.samples < 1 or self.warmups < 0:
            raise ValueError("samples must be positive and warmups cannot be negative")


@dataclass(frozen=True, slots=True)
class Sample:
    duration_ms: float
    success: bool
    error_type: str | None = None


@dataclass(frozen=True, slots=True)
class Summary:
    count: int
    successes: int
    failures: int
    error_rate: float
    minimum: float | None
    maximum: float | None
    p50: float | None
    p90: float | None
    p95: float | None
    p99: float | None


@dataclass(frozen=True, slots=True)
class Series:
    spec: SeriesSpec
    samples: tuple[Sample, ...]
    summary: Summary


@dataclass(frozen=True, slots=True)
class BenchmarkReport:
    schema_version: str
    generated_at: str
    metadata: Mapping[str, object]
    series: tuple[Series, ...]

    def to_dict(self) -> dict[str, object]:
        return asdict(self)


async def measure_series(
    spec: SeriesSpec,
    operation: Callable[[], Awaitable[None]],
    *,
    clock_ns: Callable[[], int] = time.perf_counter_ns,
) -> Series:
    for _ in range(spec.warmups):
        await operation()

    samples: list[Sample] = []
    for _ in range(spec.samples):
        started = clock_ns()
        try:
            await operation()
        except Exception as error:
            duration = _milliseconds(clock_ns() - started)
            samples.append(Sample(duration_ms=duration, success=False, error_type=type(error).__name__))
        else:
            duration = _milliseconds(clock_ns() - started)
            samples.append(Sample(duration_ms=duration, success=True))
    return Series(spec=spec, samples=tuple(samples), summary=summarize(samples))


def build_report(metadata: Mapping[str, object], series: Sequence[Series]) -> BenchmarkReport:
    identities: set[tuple[str, str, str]] = set()
    for item in series:
        identity = (item.spec.scenario, item.spec.pool, item.spec.cache_state)
        if identity in identities:
            raise ValueError(f"duplicate benchmark series {identity}")
        identities.add(identity)
    return BenchmarkReport(
        schema_version="1",
        generated_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        metadata=dict(metadata),
        series=tuple(series),
    )


def summarize(samples: Sequence[Sample]) -> Summary:
    successful = sorted(sample.duration_ms for sample in samples if sample.success)
    failures = len(samples) - len(successful)
    return Summary(
        count=len(samples),
        successes=len(successful),
        failures=failures,
        error_rate=failures / len(samples) if samples else 0.0,
        minimum=successful[0] if successful else None,
        maximum=successful[-1] if successful else None,
        p50=_percentile(successful, 50),
        p90=_percentile(successful, 90),
        p95=_percentile(successful, 95),
        p99=_percentile(successful, 99),
    )


def render_markdown(report: BenchmarkReport) -> str:
    lines = [
        "# Sandbox benchmark report",
        "",
        f"Generated: `{report.generated_at}`  ",
        f"Schema: `{report.schema_version}`",
        "",
        "> Benchmark measurements are environment-specific. Acceptance-test observations are not SLOs.",
        "",
        "## Environment",
        "",
    ]
    for key, value in sorted(report.metadata.items()):
        lines.append(f"- **{key}**: `{value}`")
    lines.extend(
        [
            "",
            "## Results",
            "",
            "| Scenario | Pool | Cache | Samples | Errors | Min ms | p50 | p90 | p95 | p99 | Max ms |",
            "| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
        ]
    )
    for item in report.series:
        summary = item.summary
        lines.append(
            "| "
            + " | ".join(
                [
                    item.spec.scenario,
                    item.spec.pool,
                    item.spec.cache_state,
                    str(summary.count),
                    f"{summary.failures} ({summary.error_rate:.1%})",
                    _format(summary.minimum),
                    _format(summary.p50),
                    _format(summary.p90),
                    _format(summary.p95),
                    _format(summary.p99),
                    _format(summary.maximum),
                ]
            )
            + " |"
        )
    return "\n".join(lines) + "\n"


def _percentile(values: Sequence[float], percentile: int) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    rank = (percentile / 100) * (len(values) - 1)
    lower = math.floor(rank)
    upper = math.ceil(rank)
    if lower == upper:
        return values[lower]
    return values[lower] + (values[upper] - values[lower]) * (rank - lower)


def _milliseconds(nanoseconds: int) -> float:
    return round(nanoseconds / 1_000_000, 3)


def _format(value: float | None) -> str:
    return "—" if value is None else f"{value:.3f}"
