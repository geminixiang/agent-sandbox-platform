---
status: accepted
---

# Make leases the core execution primitive

Consumers receive expiring Leases rather than ownership of Sandboxes, allowing the Platform to replace failed or rescheduled infrastructure without changing the Consumer's temporary usage right. Release or expiry terminates that right; one-shot execution can be added later as a convenience over the same Lease lifecycle rather than a second execution system.
