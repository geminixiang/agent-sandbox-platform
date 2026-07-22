---
status: accepted
---

# Enforce tenant scope below the consumer boundary

Every Lease, Workspace, and creation-retry record belongs to the indivisible Tenant Scope `(Consumer, Subject)`. The Consumer authenticates as an application and asserts an opaque short-lived Subject identity, but the Platform independently enforces that scope on every operation; knowing another Subject's resource ID must never reveal whether it exists or permit access. This deliberately protects against routing bugs inside a trusted Consumer such as mikan.
