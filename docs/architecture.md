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

## State machine (`internal/limiter`)

The state machine applies committed commands to application state. Its contract
is **determinism**: the same command sequence, in the same order, produces
byte-identical state on every node (verified by comparing `Store.DebugDump`
output across two independently built stores).

### Rules that guarantee determinism

- **Integer-only token math.** Balances are milli-tokens (1000 = one token).
  Refill uses integer division and preserves the remainder; reaching capacity
  resets the remainder so stale fractional credit cannot exceed capacity.
  Multiplications are overflow-checked; an overflow (only reachable after an
  absurdly long idle period) deterministically fills to capacity.
- **No local clock in apply.** Every command carries a leader-observed
  `timestamp_ms`. Effective time is `max(command_time, last_refill)`, so time
  never moves backward after a leader change.
- **No randomness, goroutines, network, or environment access** on the apply
  path. Raw API keys are generated on the leader *before* proposing; only the
  key prefix and HMAC digest are replicated.

### Commands

Versioned envelope (`{schema_version, id, type, timestamp_ms, payload}`) with
types: `create_policy`, `update_policy`, `set_policy_active`, `create_client`,
`revoke_client`, `decide`, and `prune`.

### Apply outcomes

`StateMachine.Apply` runs one command inside a single `StateTx` that also
advances `last_applied`. **Infrastructure errors** (storage/corruption) are
returned as Go errors and roll the transaction back for retry. **Business
rejections** (validation, version conflict, duplicate, unknown/inactive policy,
revoked client, rate-limited denial, idempotency conflict) are reported in the
`Result` — they still commit and advance `last_applied`, so a consumed entry is
never retried forever.

### Decision flow

1. Validate the request shape (policy id, subject, request id, cost).
2. **Idempotency lookup before any state change.** A matching record replays the
   original result (`duplicate: true`) with no deduction; a reused key with
   different content is an `idempotency_conflict`.
3. Authorize the client (exists, active, allowed to use the policy) and load the
   policy (exists, active, cost within `max_cost`).
4. Load or initialize the bucket (a new subject starts full), refill to the
   command time, then spend or deny.
5. Store the `DecisionRecord` (idempotency) and an `AuditEvent` (keyed by log
   index, subject stored only as a short hash).

### Policy update conversion

Editing a policy deterministically visits every existing bucket for that policy,
refills it **under the old policy** through the update timestamp, clamps it to
the new capacity, resets the remainder if the interval changed, stamps the new
policy version, and stores the new policy — all in one transaction. This keeps a
new refill rate from being applied retroactively to already-elapsed time.

### Pruning

A `prune` command removes idempotency records whose `expires_at_ms` is at or
before the command timestamp and trims audits to the newest N by log index —
deterministically, using the command timestamp, never a local clock.

## Consensus (`internal/raft`)

The Raft-lite node is built up over Phases 4–6. Phase 4 establishes the
foundation: message types, the concurrency model, persistent term/vote
transitions, recovery, and status. Elections (Phase 5) and log replication /
commit / apply (Phase 6) build on it.

### Concurrency model

A node runs a **single event-loop goroutine** that exclusively owns all mutable
consensus state (role, term, vote, leader, commit/applied indexes, and — when
leader — per-follower next/match indexes). Nothing else reads or writes those
fields. Callers interact only through channels:

- incoming `RequestVote` / `AppendEntries` RPCs are handed to the loop with a
  buffered reply channel;
- `Status()` sends a reply channel and receives an immutable snapshot;
- `Stop()` cancels the loop's context and waits for it to exit.

This channel discipline (rather than a shared mutex) is what keeps consensus
state race-free, and it means disk writes happen on the loop goroutine in short
transactions, never while a network call is in flight.

### Narrow interfaces

The node depends on small injected interfaces, so it is testable without real
networking or wall-clock time:

- `Store` — persistence (satisfied by `*storage.Store`).
- `Transport` — peer RPC sending (used from Phase 5).
- `ApplyFunc` — applying a committed entry (wired in Phase 6).
- `Clock` — `Now` / `After`, so tests drive election timing deterministically.

### Persistent transitions

- `becomeFollower(term, leaderID)` — on a **higher term** it persists the new
  term and a **cleared vote before responding**, which prevents voting twice in
  one term across a crash. The in-memory term is advanced only after the disk
  write succeeds, so memory never runs ahead of disk.
- `becomeCandidate` — increments the term and votes for self, persisting both
  before any RequestVote is sent (the sending side is Phase 5).
- `becomeLeader` — initializes each follower's `nextIndex = lastLogIndex + 1`
  and `matchIndex = 0` (the no-op append and heartbeats are Phase 5).

### Recovery and identity

On construction the node reloads term, vote, log bounds, and the commit/applied
checkpoints from the store, and pins the cluster ID: it adopts the configured ID
on first start, and **refuses to start** if the stored ID differs from the
configured one (guarding against cross-cluster mix-ups).

### RPC handling (Phase 4 baseline)

`RequestVote` and `AppendEntries` handlers enforce the term rules and
higher-term step-down and return the current term.

### Leader election (Phase 5)

- **Randomized election timeouts** (default 800–1400 ms) are drawn from a
  per-node RNG seeded independently, so nodes rarely time out together and split
  the vote. The timeout fires through the injectable `Clock`.
- A follower or candidate that times out **increments its term, votes for
  itself (persisting both first), and sends `RequestVote` to all peers
  concurrently**. Each send runs in its own goroutine and reports back to the
  event loop through a channel; the goroutines read only immutable snapshots, so
  the loop keeps sole ownership of mutable state.
- A node **grants at most one vote per term**, and only when the candidate's log
  is at least as up-to-date (last term first, then last index). Granting a vote
  resets the election timer.
- On reaching a **majority (2 of 3)**, the candidate becomes leader,
  initializes each follower's `nextIndex`/`matchIndex`, appends a **current-term
  no-op** to assert leadership, and starts sending **heartbeats** (empty
  `AppendEntries`) every heartbeat interval.
- The election timer is **reset only** on a valid `AppendEntries` from the
  current leader or when granting a vote — never on a RequestVote that is not
  granted — which avoids election starvation.
- A node **steps down immediately** on any request or response carrying a higher
  term, persisting the new term and clearing its vote before responding.

### Internal RPC transport and endpoint (Phase 5)

`internal/api` provides both sides of the wire:

- `HTTPTransport` (client) implements `raft.Transport`, POSTing JSON RPCs to a
  peer's base URL with the cluster bearer token.
- `InternalRaftHandler` (server) serves `POST /internal/raft/request-vote`,
  `POST /internal/raft/append-entries`, and `GET /internal/raft/status`, guarded
  by: constant-time **cluster bearer-token** check, a **source-node allowlist**,
  JSON **content-type** enforcement, a **64 KiB body limit**, and strict
  decoding (unknown fields rejected). These routes must never be exposed through
  the public gateway.

The node is wired into the running server when the public decision API needs it
(Phase 7).

### Log replication, quorum commit, and ordered apply (Phase 6)

**Proposals.** `Propose` is accepted only on the leader. The command is appended
to the leader's durable log first, a waiter is registered for that index, and
replication is triggered. The call returns the state machine's result only after
the entry is **committed and applied**. A caller timeout returns
`ErrProposalTimeout` but does **not** cancel the entry — it may still commit and
apply later, and the client recovers the result by retrying with the same
idempotency key. On step-down, pending waiters fail with `ErrNotLeader` (their
uncommitted entries may be overwritten).

**Replication.** The leader sends `AppendEntries` from each follower's
`nextIndex` on every heartbeat and whenever a proposal arrives. A follower checks
that it holds `prevLogIndex`/`prevLogTerm`; on mismatch it returns **conflict
hints** (`conflictTerm` + the first index of that term, or `conflictIndex =
lastIndex+1` when its log is too short) so the leader backs up efficiently. On a
match, the follower **transactionally** truncates any conflicting suffix and
appends the new entries (`OverwriteEntries`), then advances its commit index to
`min(leaderCommit, lastMatched)`.

**Commit.** The leader advances `commitIndex` to the highest index replicated on
a **majority**, subject to the **current-term rule**: an entry is committed by
count only if it belongs to the leader's current term. An older-term entry
becomes committed indirectly once a current-term entry above it does. The
new-term no-op appended on election is what lets a fresh leader carry prior-term
entries forward.

**Apply.** Committed entries are applied strictly in order, from
`lastApplied+1` through `commitIndex`. Each entry is applied via the injected
`ApplyFunc`, whose contract is to advance `last_applied` **atomically** with the
entry's effects — for a command, `limiter.ApplyEntry` does the application writes
and the checkpoint in one bbolt transaction; for a no-op/sentinel it just
advances the checkpoint. The applied result is delivered to any registered
waiter.

**Restart replay.** On construction the node replays committed-but-unapplied
entries (`lastApplied+1..commitIndex`) before serving requests, so a node that
crashed between commit and apply recovers its full state.

## Public decision API (`internal/api`, Phase 7)

`POST /v1/decisions` is the public entry point, callable on any node.

- **Auth.** The leader authenticates the client API key: it parses the
  `qlk_<prefix>_<secret>` key, looks the client up by prefix, recomputes the
  HMAC-SHA256 of the whole key with `QL_API_KEY_PEPPER`, and compares it to the
  stored digest in **constant time**. Failures return a generic 401 (no oracle),
  including a revoked client.
- **Validation.** JSON content-type, a required `Idempotency-Key`, a 64 KiB body
  cap with unknown fields rejected, and format checks on policy id, subject,
  cost, and idempotency key (422 before proposing).
- **Forwarding.** A follower forwards the request to the current leader over the
  private network, preserving `Authorization`, the idempotency key, and the
  original content-type, and marking it `X-QL-Forwarded` so it is never forwarded
  twice (loop prevention). With no known leader it returns 503 + `Retry-After`.
- **Propose & map.** The leader builds a decision command (stamping its observed
  time), proposes it, and waits for the committed apply result, which it maps to
  stable HTTP: 200 allowed, 429 denied (+`Retry-After`), 404/409/422/403 for
  rejections, 503 for no leader/quorum, and 504 for a proposal timeout. Every
  response carries `X-Request-ID`. A 504 caller must retry with the same
  idempotency key.

The `pkg/client` Go client mirrors this: it retries 503/504/network errors only
when an idempotency key is supplied, since a keyless retry could double-charge.

### Wiring

`main` now constructs the state machine, the HTTP transport, and the Raft node,
then serves `/health/live`, `/v1/decisions`, the admin routes, and the private
`/internal/raft/*` routes from one HTTP server. The node is stopped before
storage closes on shutdown.

## Admin authentication and mutation APIs (Phase 8)

- **Sign-in** compares `QL_ADMIN_TOKEN` in constant time and issues an 8-hour
  signed session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` in production)
  carrying a random CSRF token. All nodes share `QL_SESSION_KEY`, so a session
  works on any node. Failed sign-ins are throttled to five per IP per minute.
- **Mutations** require a valid session, a same-origin request, and a matching
  `X-CSRF-Token` (double-submit against the signed session). Admin responses set
  `Cache-Control: no-store` and security headers.
- **Policy and client APIs** propose Raft commands and map the applied result to
  HTTP: policy create (201) / update / activation (with `expected_version`
  optimistic concurrency → 409 on conflict), and client create / revoke. Client
  creation generates the raw key on the leader, replicates only the prefix and
  HMAC digest, and returns the raw key **once**. Admin mutations write redacted
  audit events keyed by log index; they contain no secrets.

See `docs/security.md` for the full security model.

## Testing (`internal/testcluster`)

An in-process cluster wires real storage and state machines behind a
`FaultTransport` whose directed links can be cut to simulate (possibly
asymmetric) partitions. Integration tests cover normal replication, a follower
down, an isolated leader that cannot commit, follower catch-up after a
partition heals, old-leader conflict-suffix repair, restart recovery, hot-bucket
concurrency (never over-allowing), and application-state convergence.
