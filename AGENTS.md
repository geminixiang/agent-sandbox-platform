# Repository guidance

- Keep `packages/sdk-typescript` free of runtime dependencies.
- Kubernetes, CRD, Helm, router, and runtime concerns belong behind the control-plane backend seam.
- The local process backend is development-only and must never be described as secure isolation.
- Every protocol change requires contract, SDK, and control-plane tests.
- Every SDK release must pass pack-install-import verification.
- Use ESM and Node.js >=22.19.0.
