# Colima gVisor bounded streaming report

Generated: `2026-07-22T20:17:55Z`  
Implementation commit: `d009cd37a7dc83357af3b65a783707e06aef6474`

> These measurements are environment-specific descriptive observations, not SLOs or performance guarantees.

## Result

**PASS**

- Built-wheel Python acceptance passed for both coding and browser Pools.
- Each Pool streamed a 32 MiB binary payload in 64 KiB async chunks, with incremental byte-count and SHA-256 verification.
- Six benchmark series produced 18 successful samples and zero failures.
- Raw benchmark JSON validated against `tests/benchmark/result.schema.v1.json`.

## Environment

- Colima: 2 vCPU, 4 GiB RAM, one arm64 VM/node
- Kubernetes: `v1.35.0+k3s1`
- Agent Sandbox: `v0.5.2`
- gVisor: `20260714.0`
- Coding image: `sha256:1e6408ccd0a60f9677da2e5d136b968b3878788e8676e66398e622b943d8acf3`
- Browser image: `sha256:fcaa293aac208973163933277fc2fbbd9321397311bd359b97de07de2cec88a2`
- WarmPools: one ready coding replica and one ready browser replica
- Transfer limit: 64 MiB
- Transfer concurrency used by acceptance: global 2, per Lease 1
- Public path: clean-installed built Python wheel → Go HTTP control plane → Kubernetes SPDY exec → ASP1 runtime helper → gVisor Sandbox

The acceptance runner recycled both mutable local WarmPools and required each running Pod `imageID` to match the image built for the run before testing.

## Acceptance coverage

The public Python SDK path verified:

- coding and browser 32 MiB raw binary upload/download;
- lazy 64 KiB async chunks rather than whole-payload SDK buffering;
- incremental length and SHA-256 integrity;
- existing destination remains unchanged while upload is incomplete;
- wrong digest returns `422 CONTENT_DIGEST_MISMATCH` and preserves the old destination;
- download early-close releases the transfer permit;
- release during upload aborts with typed `ABORTED` and completes Lease cleanup;
- cross-Subject stream reads and writes are indistinguishable from unknown Lease IDs;
- symlink escape reads/writes return `400 INVALID_PATH` and do not modify the outside sentinel;
- no `.asp-upload-*` or `.asp-download-*` artifacts remain;
- Claims before and after the run are both empty;
- both WarmPools recover to `1/1` ready.

A malformed short HTTP body is covered at the control-plane contract seam rather than by the real-cluster runner because intentionally broken HTTP/1 framing is transport-dependent and produced a fragile E2E signal.

## Benchmark methodology

- Streaming scenarios are independent from legacy JSON/base64 scenarios and are never combined into one percentile series.
- Sizes: 1 MiB, 10 MiB, and 32 MiB.
- Direction: upload and download.
- Chunk size: 64 KiB.
- Samples: 3 recorded samples plus 1 excluded warm-up per streaming series.
- Fixture preparation occurs outside the timed region.
- Each timed operation includes built-wheel SDK, HTTP, control plane, SPDY, runtime helper, storage, and incremental integrity verification.

## Streaming results

| Scenario | Samples | Errors | Min | p50 | p95 | Max | Descriptive p50 throughput |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| write 1 MiB | 3 | 0 | 49 ms | 63 ms | 288 ms | 313 ms | 15.8 MiB/s |
| read 1 MiB | 3 | 0 | 398 ms | 586 ms | 593 ms | 594 ms | 1.7 MiB/s |
| write 10 MiB | 3 | 0 | 400 ms | 509 ms | 579 ms | 587 ms | 19.6 MiB/s |
| read 10 MiB | 3 | 0 | 392 ms | 522 ms | 665 ms | 680 ms | 19.1 MiB/s |
| write 32 MiB | 3 | 0 | 507 ms | 577 ms | 613 ms | 616 ms | 55.5 MiB/s |
| read 32 MiB | 3 | 0 | 878 ms | 882 ms | 1013 ms | 1028 ms | 36.3 MiB/s |

The unusually low 1 MiB read throughput reflects fixed request, SPDY, snapshot, and digest overhead on this local environment. Three samples are deliberately low-cost smoke evidence; p90/p95/p99 are interpolated descriptions, not statistically meaningful tail estimates.

## Failures discovered and corrected

No failed or rejected run is treated as baseline evidence.

1. **Visible download snapshot after early close**  
   Kubernetes exec cancellation can return before the Sandbox process exits. The runtime helper originally removed its snapshot only on process exit, leaving a temporary pathname briefly visible. Commit `1c52fa2` now unlinks the snapshot immediately after opening the stable file descriptor and before streaming the body. A regression test blocks body streaming and asserts no snapshot pathname exists.

2. **Mutable local image did not roll the WarmPool**  
   Rebuilding `agent-sandbox-*:local` did not change the SandboxTemplate, so an old ready Pod could continue running. Commit `16c6994` now scales each WarmPool to zero and back to one, then checks its Pod `imageID` against containerd before acceptance.

3. **Release during upload lost typed failure semantics**  
   The server correctly cancelled the transfer, but HTTP/1 could terminate while the client was still uploading, causing `httpx.ReadError` before its JSON `408` was read. Commit `d009cd3` maps upload transport termination to typed `ABORTED` in Python and TypeScript, with SDK regression tests.

## Limitations and follow-ups

- The 64 MiB limit is currently fixed by the protocol/runtime invariant.
- A total timeout is enforced; an idle timeout is not yet implemented and is not claimed.
- The runtime helper must be present in custom Pool images.
- Streaming does not yet include filesystem list/stat/mkdir/remove/rename/watch.
- No GKE or multi-node throughput result is implied by this local report.
- Raw JSON remains gitignored under `.sandbox-platform/benchmarks/`; the secret-safe acceptance evidence remains under `.sandbox-platform/test-reports/`.

## Cleanup proof

- Benchmark and acceptance Claims: none remaining.
- Temporary control-plane and observer processes: stopped.
- Temporary wheel environment and dynamic credentials: removed.
- Coding WarmPool: `1/1` ready.
- Browser WarmPool: `1/1` ready.
- No cloud resources were created or modified.
