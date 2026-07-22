# Sandbox runtime contract

`file-transfer/main.go` builds the static `/usr/local/bin/agent-sandbox-transfer` helper included by the published coding and browser workload images.

The helper implements the private ASP1 Pod-exec protocol used by the production Kubernetes backend. It pins path traversal beneath `/workspace`, rejects symlinks and non-regular files, preflights bounded download snapshots, and validates streamed uploads before an atomic sibling rename. Its stdout contains only bounded protocol markers followed by download bytes; implementation diagnostics and temporary names are never part of the HTTP interface.

Operator-provided Pool images must install a compatible helper at the same path. This is a sandbox workload runtime contract, not an SDK dependency.
