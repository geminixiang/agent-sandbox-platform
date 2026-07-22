# Coding sandbox image

Non-root daily-work baseline for the `coding` Pool:

- Python 3.13 and pip;
- git and OpenSSH client;
- curl, jq, and CA certificates;
- writable `/workspace` owned by UID/GID 10001;
- the static `/usr/local/bin/agent-sandbox-transfer` runtime helper required for bounded production streaming.

The image intentionally excludes compilers, Docker, Kubernetes credentials, and privileged tooling. Add language-specific Pools rather than turning the baseline into an unbounded development VM.
