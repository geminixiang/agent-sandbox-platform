# Control plane image

Static, non-root production image for the Go control plane.

```bash
docker build -f images/control-plane/Dockerfile -t agent-sandbox-control-plane:local .
```

The binary uses in-cluster Kubernetes configuration by default and fails closed if Kubernetes or required configuration is unavailable.
