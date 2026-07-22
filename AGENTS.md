# Repository guidance

- Keep `packages/sdk-typescript` free of runtime dependencies.
- Kubernetes, CRD, Helm, router, and runtime concerns belong behind the control-plane backend seam.
- The local process backend is development-only and must never be described as secure isolation.
- Every protocol change requires contract, SDK, and control-plane tests.
- Every SDK release must pass pack-install-import verification.
- Treat every production-like, cloud, or real-cluster test as durable engineering evidence. After each run, create or update a GitHub issue with the environment and versions, exact scope, results and measurements, failures and diagnoses, fixes, cleanup proof, and follow-up issues. Never include credentials or tokens.
- Add every durable test-report issue to `docs/test-reports.md`; keep detailed reports in issues rather than turning `AGENTS.md` into a changelog.
- Performance observations from acceptance tests are not benchmarks or SLOs unless produced by the benchmark methodology and labeled as such.
- Use ESM and Node.js >=22.19.0.
