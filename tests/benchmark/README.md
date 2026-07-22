# Benchmark harness

## Colima baseline

Run a built-wheel local baseline against the pinned Colima+k3s+gVisor environment:

```bash
./scripts/benchmark/colima.sh
```

The runner records warm acquisition (only `create()` is timed), WarmPool replenishment, bounded concurrency, coding exec/file operations, six coding streaming-transfer series, and browser milestones. Cleanup and readiness waits occur outside acquire timing. Raw JSON and generated Markdown are written under the gitignored `.sandbox-platform/benchmarks/` directory for review.

The default run uses 10 samples plus 2 warm-ups for legacy single-operation series and a bounded 3 samples plus 1 warm-up for concurrency 1/2/4. Streaming writes and fully consumed reads are separate 1, 10, and 32 MiB series with 3 samples plus 1 warm-up by default. Override only their recorded sample count with `SANDBOX_BENCHMARK_STREAM_SAMPLES`. Upload chunks are generated lazily at 64 KiB, fixtures are prepared outside read timing, and byte counts plus SHA-256 are verified incrementally without materializing the complete payload. The smaller concurrency count keeps the single-node, one-replica local baseline within a practical time budget; it is not a throughput or capacity SLO.

The legacy JSON file-operation series remain unchanged: raw binary writes are measured through 512 KiB and reads through 7 MiB because base64 expansion reaches the 1 MiB JSON request limit and 10 MiB command-output limit. The streaming series exercise the binary transfer protocol instead.

## Result contract

The benchmark module measures the public SDK path and emits versioned raw JSON plus a Markdown summary. It keeps each `(scenario, pool, cache_state)` in a separate series so warm and cold samples cannot be combined accidentally.

Example:

```bash
SANDBOX_PLATFORM_URL=http://127.0.0.1:8787 \
SANDBOX_PLATFORM_TOKEN="$SHORT_LIVED_SUBJECT_TOKEN" \
python -m tests.benchmark.warm_acquire \
  --pool coding --pool browser \
  --samples 10 --warmups 2 \
  --metadata /tmp/benchmark-metadata.json \
  --json-output /tmp/result.json \
  --markdown-output /tmp/result.md
```

Requirements:

- use a built and clean-installed Python SDK for recorded baselines;
- use a short-lived Subject token and never write it to report metadata;
- record the immutable git commit, Kubernetes `imageID` values, and environment versions;
- treat acceptance-test timings as observations, not benchmark baselines;
- retain raw JSON as an artifact and commit only reviewed Markdown baseline reports.

`result.schema.v1.json` is the canonical result contract. Schema changes require a new version rather than silently changing prior benchmark artifacts.
