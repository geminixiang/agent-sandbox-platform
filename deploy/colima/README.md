# Colima development environment

Reserved for an isolated local Kubernetes environment used by control-plane integration tests.

The target environment requires:

- containerd
- gVisor or Kata RuntimeClass
- Agent Sandbox controller and CRDs
- a development WarmPool
- the platform control plane

Phase 0 does not provision these resources yet. Use the local process backend only for trusted contract development.
