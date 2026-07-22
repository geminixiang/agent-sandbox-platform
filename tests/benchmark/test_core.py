from __future__ import annotations

import asyncio

import pytest

from .core import Sample, SeriesSpec, build_report, measure_series, render_markdown, summarize


def test_summary_uses_interpolated_percentiles_and_excludes_failures() -> None:
    summary = summarize(
        [
            Sample(1.0, True),
            Sample(2.0, True),
            Sample(3.0, True),
            Sample(4.0, True),
            Sample(9.0, False, "TimeoutError"),
        ]
    )
    assert summary.count == 5
    assert summary.successes == 4
    assert summary.failures == 1
    assert abs(summary.error_rate - 0.2) < 0.0001
    assert summary.p50 is not None and abs(summary.p50 - 2.5) < 0.0001
    assert summary.p90 is not None and abs(summary.p90 - 3.7) < 0.0001
    assert summary.maximum == 4.0


@pytest.mark.asyncio
async def test_measure_series_excludes_warmups_and_records_errors() -> None:
    invocations = 0
    ticks = iter(range(0, 100_000_000, 1_000_000))

    async def operation() -> None:
        nonlocal invocations
        invocations += 1
        if invocations == 3:
            raise RuntimeError("sample failure")

    series = await measure_series(
        SeriesSpec("acquire", "coding", "warm", samples=3, warmups=1),
        operation,
        clock_ns=lambda: next(ticks),
    )
    assert invocations == 4
    assert len(series.samples) == 3
    assert [sample.success for sample in series.samples] == [True, False, True]
    assert all(sample.duration_ms == 1.0 for sample in series.samples)
    assert series.summary.failures == 1


def test_report_rejects_duplicate_series_identity() -> None:
    spec = SeriesSpec("acquire", "coding", "warm", samples=1)
    series = asyncio.run(measure_series(spec, _noop))
    with pytest.raises(ValueError, match="duplicate benchmark series"):
        build_report({}, [series, series])


def test_markdown_keeps_cache_state_visible() -> None:
    warm = asyncio.run(measure_series(SeriesSpec("acquire", "coding", "warm", 1, 0), _noop))
    cold = asyncio.run(measure_series(SeriesSpec("acquire", "coding", "image-uncached", 1, 0), _noop))
    rendered = render_markdown(build_report({"commit": "abc123"}, [warm, cold]))
    assert "| acquire | coding | warm |" in rendered
    assert "| acquire | coding | image-uncached |" in rendered
    assert "Acceptance-test observations are not SLOs" in rendered


async def _noop() -> None:
    return None
