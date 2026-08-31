# ADR 0003: bbolt for per-node durable storage

## Status

Accepted

## Context

Each node needs durable local storage for Raft metadata (current term, vote),
the replicated log, and the applied application state (policies, clients,
buckets, idempotency records, audits). Consensus safety depends on writing term
and vote before responding, and restart recovery depends on exact log ordering.

We want to avoid running a separate database server so a reviewer can start the
whole system with only Docker.

## Decision

Use **go.etcd.io/bbolt v1.5.0**, an embedded key/value store, with **one file
per node**. Replication keeps logical state aligned across nodes via the Raft
log — nodes never share a file.

- All writes are transactional; a failed transaction commits none of its changes.
- `uint64` keys (e.g. log indexes) are stored as fixed 8-byte big-endian values
  so they sort numerically.
- Only two components may write: the Raft persistence layer (term, vote, log,
  checkpoints) and the state machine (application buckets) while applying
  committed commands. HTTP handlers never write application buckets directly.
- An entry's application writes and the new `last_applied` checkpoint commit in
  the **same** transaction, so a crash leaves either both or neither.

## Consequences

- No external database process; fully self-contained per node.
- Encoding lives in one place (`internal/storage/encoding.go`).
- The log grows without compaction (see ADR 0001); backups and size monitoring
  are part of deployment.

## Alternatives considered

- **External database (PostgreSQL) or Redis**: rejected; adds an external
  dependency and undercuts the "runs with only Docker" goal.
- **Custom file format**: rejected; bbolt provides transactions and durability
  without reinventing them.
