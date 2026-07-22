# Test report index

Production-like, cloud, and real-cluster tests are durable engineering evidence. The linked GitHub issues are the canonical reports; this file keeps them discoverable from the repository.

## Reporting requirements

Every report must record:

- date and final pass/fail status;
- provider, cluster topology, region/zone, relevant software versions, image tags or digests, and commit SHA;
- scenarios exercised and important negative/failure cases;
- measurements, with informal observations clearly distinguished from benchmarks and SLOs;
- failures encountered, diagnosis, and fixes made;
- final resource, policy, credential, and cluster cleanup state;
- related commits, CI runs, benchmark work, and follow-up issues.

Never record credentials, tokens, Secret values, or private workload data. Update an existing issue when repeating the same investigation; create a new report when the environment, release candidate, scope, or conclusions materially differ.

## Reports

| Issue | Environment | Result | Summary | Related work |
| --- | --- | --- | --- | --- |
| [#2 — GKE gVisor daily-work acceptance test report](https://github.com/geminixiang/agent-sandbox-platform/issues/2) | Dedicated three-node GKE Standard cluster, Agent Sandbox v0.5.2, gVisor coding/browser Pools | Pass; cluster and local credentials removed | Built-wheel Python coding, crawler, Git/HTTPS, Playwright, isolation, timeout, readiness, policy restoration, and lifecycle cleanup | [#1 — reproducible performance benchmarks](https://github.com/geminixiang/agent-sandbox-platform/issues/1) |

## Benchmark tracking

- [#1 — Establish reproducible sandbox performance benchmarks](https://github.com/geminixiang/agent-sandbox-platform/issues/1) defines the methodology needed before latency observations become baselines or SLO inputs.
