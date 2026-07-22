# Benchmark harness

## Colima baseline

Run a built-wheel local baseline against the pinned Colima+k3s+gVisor environment:

```bash
./scripts/benchmark/colima.sh
```

The runner records warm acquisition (only `create()` is timed), WarmPool replenishment, bounded concurrency, coding exec/file operations, and browser milestones. Cleanup and readiness waits occur outside acquire timing. Raw JSON and generated Markdown are written under the gitignored `.sandbox-platform/benchmarks/` directory for review.

Current protocol limits mean raw binary writes are measured through 512 KiB and reads through 7 MiB: base64 expansion reaches the 1 MiB JSON request limit and 10 MiB command-output limit before raw payloads reach those limits.

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
- record immutable image digests and environment versions;
- treat acceptance-test timings as observations, not benchmark baselines;
- retain raw JSON as an artifact and commit only reviewed Markdown baseline reports.

`result.schema.v1.json` is the canonical result contract. Schema changes require a new version rather than silently changing prior benchmark artifacts.
