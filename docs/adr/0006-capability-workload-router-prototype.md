# ADR 0006: Gate workload ingress on a capability router

- Status: Accepted for isolated prototype; public contract pending gate evidence
- Date: 2026-07-23

## Context

Agent workloads commonly start HTTP development servers, previews, callback receivers, or WebSocket endpoints inside a Sandbox. Consumers need a simple logical interface such as `sandbox.ports.expose(3000)` without learning Pod IPs, Services, namespaces, Ingress resources, or cloud load balancers.

The cluster currently has no production workload router, Gateway API dependency, or Ingress controller requirement. Agent Sandbox secure-default networking allows ingress only from a future `app=sandbox-router` workload in the controller namespace, but no such router is installed. Publishing `get_url()` before validating routing, authentication, NetworkPolicy, WebSocket, and revocation would freeze semantics that the runtime may not support portably.

The platform Subject token must never be placed in a browser URL, workload request, router log, or Sandbox process. A router must not become an arbitrary cluster proxy or permit Consumers to select Pod IPs, Services, namespaces, or unapproved ports.

## Decision

Before adding public Exposure endpoints or SDK methods, build an isolated workload-router prototype gate.

The prototype must not change:

- `/v1` HTTP contracts;
- Python or TypeScript SDKs;
- production Go backend interfaces;
- Helm defaults;
- existing coding/browser Pools;
- secure-default egress policy.

### Routing architecture

The target architecture is:

```text
external HTTPS/WSS
  → shared workload router
  → one platform-managed ClusterIP Service per Exposure
  → one active Sandbox Pod and one operator-approved target port
```

The router cannot choose arbitrary destinations. It only accepts an opaque Exposure host and resolves it through trusted platform state to a platform-managed Service. Kubernetes selectors, Pod identity, namespace, and Service names remain internal implementation details.

The prototype may use isolated in-memory control state, but it must exercise the same Host-routing and Service data path. It must not use `kubectl port-forward` as workload ingress.

### Pool policy

Ingress is secure-default disabled. A future Pool policy owns:

- enabled/disabled;
- allowed HTTP ports;
- whether WebSocket Upgrade is permitted;
- default and maximum Exposure TTL;
- maximum Exposures per Lease;
- maximum connections per Exposure;
- private or internet-reachable gateway class.

Consumers cannot declare arbitrary protocols, visibility, gateways, or ports. MVP supports external HTTPS/WSS to cleartext HTTP inside the Sandbox. Arbitrary TCP/UDP is deferred.

### Browser authentication

The returned browser URL contains a one-time, short-lived bootstrap capability in the URL fragment:

```text
https://<opaque-exposure-host>.<router-domain>/#asp=<capability>
```

Fragments are not sent in HTTP requests, DNS, TLS SNI, proxy access logs, or Referer headers. An unauthenticated request receives a fixed bootstrap document with restrictive CSP. The document reads the fragment and posts the capability in the request body to the same host. Successful one-time exchange sets a host-only `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/` session cookie and uses `location.replace("/")` to remove the fragment from browser history.

Subsequent same-origin HTTP and WebSocket requests use the cookie. A bare WebSocket handshake before HTTP bootstrap is unsupported. Iframes and third-party-cookie flows are deferred.

The bootstrap capability is not a platform Subject token. It is independent, single-use, short-lived, exposure-bound, and stored only as a hash. Logs, Kubernetes metadata, errors, and reports must not contain raw capabilities or session cookies.

### Kubernetes resources and isolation

A future production Exposure creates one ClusterIP Service with:

- an operator-approved target port;
- a selector derived from authoritative Sandbox status, not guessed Pod names;
- an owner reference that permits garbage collection with the Lease/Claim;
- only hashed platform metadata;
- no raw Consumer, Subject, Lease capability, or session value.

NetworkPolicy must be additive to upstream secure defaults:

- Sandbox ingress only from the router namespace/label and only to approved Pool ports;
- router egress only to its control-plane resolver, DNS, and approved Sandbox targets;
- no arbitrary host/IP/port proxying.

### Lifecycle semantics

An Exposure has an expiry no later than its Lease. Revoke, release, delete, and expiry must:

1. reject new requests and upgrades;
2. invalidate bootstrap capabilities and sessions;
3. close existing WebSocket and long-lived HTTP connections within a documented maximum latency;
4. remove the managed Service;
5. preserve normal Lease cleanup.

Router or control-plane restart must not silently reactivate revoked state. Prototype state may be ephemeral, but production design must include recovery and stable secrets before release.

## Prototype gate

A dedicated Colima namespace, router image, workload fixture, Service, and NetworkPolicies must verify under the pinned Kubernetes/gVisor environment:

1. Host-based routing preserves root paths, assets, query strings, redirects, and WebSocket Upgrade.
2. The bootstrap capability is absent from router access logs, workload headers, Referer, Kubernetes resources, and evidence.
3. Capability exchange is one-time and expiry-bound; invalid, replayed, or cross-Exposure values fail indistinguishably.
4. Host-only session cookies authorize HTTP and same-origin WebSocket requests.
5. Unknown hosts, malformed Host headers, unapproved ports, IP literals, and destination-like paths cannot produce arbitrary proxying.
6. The per-Exposure Service selects only the intended Sandbox Pod and approved target port.
7. Sandbox ingress from a non-router Pod is denied while router ingress succeeds.
8. Revocation rejects new traffic and closes an established WebSocket within the measured bound.
9. Router restart does not leak or broaden access.
10. Prototype namespace, Services, Pods, Claims, PVCs, policies, and Secrets are removed; existing Pools remain unchanged.

Any unexpected failure blocks contract publication. A passed prototype does not itself create production support; it only permits the contract/backend stage to begin.

## Deployment seam

Production Helm/operator configuration will eventually own:

- router enablement;
- base domain;
- wildcard TLS Secret reference;
- Service type and provider annotations;
- internal/external gateway class;
- trusted proxy CIDRs;
- connection, header, body, handshake, and idle limits;
- Pool-to-gateway mapping.

The platform will not automatically create DNS records, certificates, cert-manager, Gateway API, or provider-specific load balancers.

## Deferred

- arbitrary TCP/UDP tunnels;
- anonymous public exposure;
- Consumer custom domains;
- workload-internal TLS;
- SDK-controlled egress policy;
- iframe/third-party-cookie support;
- Gateway API and Ingress controller adapters;
- multi-replica distributed connection/session registry.

## Consequences

The router adds a separate data-plane module and operating surface, but keeps untrusted workload responses and long-lived WebSockets away from the management control plane. Per-Exposure Services add Kubernetes objects and propagation latency, but provide a restricted destination set that is safer than direct Pod-IP proxying.

No customer-facing ingress claim is valid until the gate passes and a subsequent production contract/backend/SDK stage is completed.
