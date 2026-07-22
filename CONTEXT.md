# Agent Sandbox Platform

A shared platform that grants agent applications controlled access to isolated execution environments without exposing infrastructure-specific lifecycle details.

## Language

**Consumer**:
An agent application that requests and uses sandboxes through the platform interface.
_Avoid_: Client, tenant, integration

**Primary Design Partner**:
The Consumer whose real workloads and feedback drive initial prioritization without making the Platform interface Consumer-specific. Mikan is the initial Primary Design Partner.
_Avoid_: Only user, owner application

**Subject**:
An opaque user identity asserted by a Consumer and independently enforced by the Platform as part of every resource ownership check. The Platform does not interpret the upstream person or account represented by it.
_Avoid_: Slack user, actor, Principal

**Tenant Scope**:
The indivisible ownership scope `(Consumer, Subject)` applied to every Lease, Workspace, and creation-retry record. Possession of a resource ID never grants access outside this scope.
_Avoid_: Consumer alone, deployment tenant, organization

**Sandbox**:
An isolated, stateful execution environment managed by the Platform as replaceable infrastructure beneath a Lease.
_Avoid_: Pod, container, VM, consumer resource

**Lease**:
A temporary right granted within one Tenant Scope to use defined sandbox capabilities until relinquished or expired. It is not the Workspace or underlying Sandbox.
_Avoid_: Sandbox session, allocation, instance, stored workspace

**Platform**:
The system that grants, tracks, recovers, and retires sandboxes while hiding execution infrastructure from Consumers.
_Avoid_: Proxy, Kubernetes wrapper
