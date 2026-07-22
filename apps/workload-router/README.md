# Workload router prototype

This directory is the isolated gate described by [ADR 0006](../../docs/adr/0006-capability-workload-router-prototype.md). It does not change the public `/v1` contract, SDKs, control-plane backend, Helm defaults, or existing Pools.

## Modules

- `internal/auth`: ephemeral Exposure registry, one-time bootstrap capabilities, host-bound sessions, connection limits, expiry, revocation, and secret-safe events.
- `internal/router`: fixed bootstrap document, cookie authentication, HTTP/WebSocket reverse proxy, header filtering, resource limits, redirect rewriting, and active connection tracking.
- `internal/resolver`: the trusted destination seam. A Resolver can return only a validated Kubernetes Service name, namespace, and HTTP port; it cannot return an arbitrary URL.

The intended production data path remains:

```text
external HTTPS/WSS terminator
  -> shared workload router
  -> platform-managed ClusterIP Service
  -> one Sandbox workload port approved by Pool policy
```

The trusted control side calls `auth.Registry.Register` directly and retains the returned raw bootstrap capability only long enough to return `router.BootstrapURL` to the Consumer. The Registry stores SHA-256 digests, not raw capabilities or sessions. `Registry.Revoke`, expiry timers, and `Registry.Close` invalidate state and close tracked upstream HTTP streams and WebSockets.

## Browser bootstrap

An unauthenticated GET to a known Exposure host returns one fixed HTML document with a hash-based restrictive CSP. The script reads `#asp=<capability>`, POSTs it in a same-origin body to `/_asp/bootstrap`, receives a host-only `__Host-asp_session` cookie (`Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`), and calls `location.replace("/")`. Bare pre-bootstrap WebSocket handshakes are rejected.

Unit tests assert the exact document, script, CSP, and cookie attributes. They do **not** claim that a browser executes URL-fragment JavaScript correctly. Real browser validation belongs in the pinned Colima gate.

## Security properties in this prototype

- Request Host values with ports, trailing dots, IP literals, userinfo, delimiters, whitespace, or invalid DNS syntax are rejected.
- Invalid, expired, replayed, and cross-Exposure capabilities receive the same external denial.
- `Authorization`, proxy authorization, the router session cookie, and `X-ASP-*` headers are removed before proxying. A workload cannot replace the reserved router cookie through `Set-Cookie`.
- Paths, queries, status, response bodies, ordinary workload cookies, external redirects, streaming, and permitted WebSocket Upgrade are preserved. Redirects to the selected internal Service are rewritten to the external HTTPS host; other `.svc` redirects fail closed.
- The resolver target type permits cleartext HTTP only and has no representation for scheme, userinfo, arbitrary hostname, path, query, or fragment.
- The prototype disables upstream keep-alive pooling so one tracked connection cannot be shared across Exposure identities. This is deliberately conservative, not a production performance design.
- Registry restart with empty in-memory state denies all old capabilities and sessions; there is no recovery or silent activation.
- Secret-safe event hooks contain only opaque Exposure ID, event type, and outcome. Event sinks must themselves be concurrency-safe.

`router.NewHTTPServer` supplies prototype header-size, read-header, and idle bounds. `router.Options.MaxRequestBodyBytes` and `BodyReadTimeout` bound proxied and bootstrap request bodies. The server deliberately has no absolute `WriteTimeout`, because that would terminate long-lived streaming responses and WebSockets; revocation and tracked-connection closure provide their lifecycle bound. Each Registration supplies a concurrent request/upgrade limit. TLS termination, wildcard DNS/certificates, trusted-proxy handling, Kubernetes Service lifecycle, NetworkPolicy, and internet/private gateway selection remain external/operator-owned.

## Run tests

```sh
go test ./apps/workload-router/...
go test -race ./apps/workload-router/...
go vet ./apps/workload-router/...
```

## Known gaps before contract publication

This is intentionally not a production deployment. It is single-replica and ephemeral; it has no distributed registry, stable session secret, network admin API, Kubernetes Service controller, Helm image, TLS listener, trusted-proxy configuration, or Colima evidence. Upstream keep-alives are disabled. The next gate must build the router/fixture image and verify real DNS, Service selectors, NetworkPolicies, browser fragment execution, WebSocket revocation latency, restart behavior, resource cleanup, and absence of secrets from logs, headers, Referer, and Kubernetes objects. Passing these unit tests does not publish workload ingress support.
