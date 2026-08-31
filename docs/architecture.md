# Architecture

This document grows as the build progresses. It currently covers the storage
layer (Phase 2). Consensus, the state machine, APIs, and the dashboard are
documented as their phases land.

## Storage (`internal/storage`)

Each node owns exactly one bbolt database file (default
`/data/quorumlimiter.db`). Nodes never share a file; the Raft log keeps their
logical state aligned. bbolt is an embedded key/value store with fully
transactional writes — a failed transaction commits none of its changes.

### Write ownership

Only two components may write, which is enforced by the package API:

1. **Raft persistence** — current term, vote, log entries, and the commit /
   applied checkpoints (`raft_store.go`).
2. **The deterministic state machine** — policies, clients, token buckets,
   idempotency records, and audits, and only through `Store.Apply`, which hands
   the callback a `StateTx` (`state_store.go`).

HTTP handlers never write application buckets directly. They propose commands
and wait for the committed apply result (Phases 6–7).

### Buckets (schema version 1)

| Bucket          | Key                                   | Value                     | Writer          |
|-----------------|---------------------------------------|---------------------------|-----------------|
| `meta`          | fixed names (schema_version, current_term, voted_for, commit_index, last_applied, cluster_id) | big-endian uint64 or UTF-8 bytes | migration, Raft |
| `raft_log`      | 8-byte big-endian log index           | JSON `LogEntry`           | Raft            |
| `policies`      | policy ID                             | encoded Policy            | state machine   |
| `clients`       | client ID                            | encoded Client            | state machine   |
| `client_prefix` | API key prefix                        | client ID                 | state machine   |
| `token_buckets` | length-prefixed policy ID + subject   | encoded TokenBucketState  | state machine   |
| `idempotency`   | length-prefixed client ID + request ID| encoded DecisionRecord    | state machine   |
| `audit`         | 8-byte big-endian log index           | encoded AuditEvent        | state machine   |

Application values are opaque encoded bytes to storage; their concrete models
and JSON encoding live in `internal/limiter` (Phase 3), so storage never imports
application types.

### Encoding (`encoding.go`)

- `uint64` keys/values are fixed 8-byte **big-endian** so log and audit keys
  sort numerically.
- Composite keys prefix each part with its 2-byte big-endian length
  (`[len][part][len][part]`) so a separator can never collide with a subject's
  own bytes. Parts longer than 65,535 bytes are rejected.
- Byte slices returned from a read are **copied**, because bbolt-owned memory is
  only valid within its transaction.

### Log sentinel

A sentinel entry at index 0, term 0 (`KindSentinel`) is installed by the version
1 migration. It simplifies previous-entry checks during replication and is never
truncated or shown in audits.

### Migrations (`migrations.go`)

On startup, `Open` runs `migrate` inside one writable transaction: it reads the
stored schema version (missing = 0), applies each migration in ascending order,
and records the new version only after each migration succeeds. Reopening an
up-to-date database makes no changes (idempotent). Startup **refuses** to
proceed if the stored version is newer than the binary supports
(`ErrSchemaTooNew`), rather than risk misinterpreting data.

### Atomic apply (Phases 3–6)

When a committed command is applied, its application writes **and** the new
`last_applied` checkpoint commit in the same `StateTx`. A crash therefore leaves
either all of an entry's effects or none, so a nonidempotent command is never
replayed after restart.
