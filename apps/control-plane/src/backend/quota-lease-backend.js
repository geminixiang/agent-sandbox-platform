export class QuotaLeaseBackend {
  constructor(delegate, limits = {}) {
    this.delegate = delegate;
    this.limits = {
      perScope: positiveLimit(limits.perScope, "perScope"),
      perConsumer: positiveLimit(limits.perConsumer, "perConsumer"),
      perPool: positiveLimit(limits.perPool, "perPool"),
    };
    this.lock = Promise.resolve();
  }

  acquire(scope, request) {
    return this.withLock(async () => {
      const replay = await this.delegate.findByIdempotencyKey?.(scope, request.idempotencyKey);
      if (replay) return { lease: replay, replayed: true };

      const active = await this.delegate.listActiveLeases();
      enforceLimit(
        active.filter((lease) => lease.scopeHash === this.delegate.scopeHash(scope)).length,
        this.limits.perScope,
        "tenant scope",
      );
      enforceLimit(
        active.filter((lease) => lease.consumerHash === this.delegate.consumerHash(scope)).length,
        this.limits.perConsumer,
        "consumer",
      );
      enforceLimit(
        active.filter((lease) => lease.record.pool === request.pool).length,
        this.limits.perPool,
        "pool",
      );
      return this.delegate.acquire(scope, request);
    });
  }

  get(...args) {
    return this.delegate.get(...args);
  }

  exec(...args) {
    return this.delegate.exec(...args);
  }

  readFile(...args) {
    return this.delegate.readFile(...args);
  }

  writeFile(...args) {
    return this.delegate.writeFile(...args);
  }

  release(...args) {
    return this.delegate.release(...args);
  }

  delete(...args) {
    return this.delegate.delete(...args);
  }

  recover(...args) {
    return this.delegate.recover?.(...args);
  }

  sweepExpired(...args) {
    return this.delegate.sweepExpired?.(...args);
  }

  close(...args) {
    return this.delegate.close(...args);
  }

  withLock(run) {
    const result = this.lock.then(run, run);
    this.lock = result.catch(() => undefined);
    return result;
  }
}

function positiveLimit(value, name) {
  if (value === undefined || value === null) return Number.POSITIVE_INFINITY;
  if (!Number.isInteger(value) || value <= 0) throw new TypeError(`${name} quota must be positive`);
  return value;
}

function enforceLimit(count, limit, target) {
  if (count >= limit) {
    throw Object.assign(new Error(`Active Lease quota exceeded for ${target}`), {
      status: 429,
      code: "LEASE_QUOTA_EXCEEDED",
    });
  }
}
