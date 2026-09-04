# QuorumLimiter

QuorumLimiter is a three-node distributed token-bucket rate limiter whose state
changes are ordered by a Raft-lite consensus implementation written from scratch
in Go.

[![CI](https://github.com/fazalsarim-art/QuorumLimiter/actions/workflows/ci.yml/badge.svg)](https://github.com/fazalsarim-art/QuorumLimiter/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-blue.svg)

> **Educational project.** It demonstrates consensus mechanics and failure
> handling with a small, readable implementation. It is not a formally verified
> production consensus library. Limitations are listed explicitly below.

## Demonstration

A two-minute walkthrough (full script in [`docs/demo.md`](docs/demo.md)):

1. Start the three-node cluster.
2. Show the leader and matching commit/apply indexes across nodes.
3. Send decisions until one is denied (bucket exhausted → `429`).
4. Stop the leader.
5. Watch a new leader take over within ~1–2 s and keep serving decisions.
6. Restart the old leader and watch it rejoin as a follower and catch up.

The `scripts/smoke.sh` and `scripts/failover.sh` scripts automate steps 2–6
against the running cluster.

> _Screenshot/GIF placeholder:_ add a leader-failover capture at
> `docs/assets/failover.gif` — see the [screenshot checklist](docs/screenshots.md).

## Why this project exists

Naive rate limiting breaks when it runs on more than one server. Give three
independent servers a "5 tokens per minute" policy and a local counter each, and
a client can quietly spend **15** tokens a minute — five per server — because no
server sees the others' grants. Sticky routing hides the problem until failover
reshuffles traffic.

QuorumLimiter removes the shared-state gap: **every** decision is proposed to a
replicated log and applied only after a **majority commits** it, in the same
order on every node. Three servers therefore enforce one global budget and can
never collectively over-allow, even across leader changes and restarts.

## Features

**Application**
- Deterministic integer token bucket (milli-tokens, remainder-preserving, no
  floats and no local clock on the apply path).
- Idempotent decision API: a repeated `Idempotency-Key` replays the original
  result without charging twice.
- Policy and client lifecycle with one-time raw API keys (only a prefix + HMAC
  digest is stored).

**Consensus (from scratch)**
- Leader election with randomized timeouts, log replication with conflict-hint
  backup, majority commit under the **current-term rule**, and strictly ordered
  apply.
- Durable term/vote/log; committed-but-unapplied entries are replayed on restart.
- Fixed three-node membership; a single event-loop goroutine owns all mutable
  consensus state (channels, not shared mutexes).

**Security**
- Constant-time API-key and admin-token checks; signed `HttpOnly`/`SameSite`
  session cookies with CSRF double-submit and same-origin checks; login
  throttling; private, token-guarded internal RPCs. See [`docs/security.md`](docs/security.md).

**Observability**
- `/health/live`, `/health/ready` (with a reason), Prometheus `/metrics` with
  strictly bounded labels, and redacted structured logs.

**Testing**
- Unit, integration, and race tests, including an in-process fault-injecting
  cluster; k6 load test; smoke and failover scripts.

## Architecture

The system diagram and the decision request-flow sequence are in
[`docs/architecture.md`](docs/architecture.md), alongside the storage schema,
the deterministic state machine, consensus internals, and the APIs. The key
choices are recorded as [ADRs](docs/adr/).

In short: any node accepts a decision; a **follower forwards to the leader**; the
leader appends the decision to its durable log, replicates via `AppendEntries`,
and returns the result only after the entry is **committed on a majority and
applied**; each node keeps its own **bbolt** database and the log keeps them
identical.

## Consistency behavior

- Writes (decisions and admin mutations) require a **leader and a quorum**.
- During a partition the service **prefers consistency over write availability**:
  the minority side returns `503` rather than risk divergent state.
- A caller timeout (`504`) may **hide a commit that lands later**; clients retry
  with the **same `Idempotency-Key`** to recover the original result safely.
- Dashboard/status reads are **operational snapshots**, not a general
  linearizable read API.

## Quick start (Docker Compose)

Runs three node containers behind a Caddy gateway, plus Prometheus.

```bash
# 1. Provide shared secrets (four independent random values).
cp deploy/.env.example deploy/.env
#    Generate each, e.g.: openssl rand -base64 48 | tr -d '\n'
#    then paste into deploy/.env (git-ignored).

# 2. Build the image and start the cluster.
docker compose -f deploy/compose.dev.yml --env-file deploy/.env up -d --build

# 3. Check health (via the gateway) and see node status.
curl -fsS http://localhost:8080/health/live
docker compose -f deploy/compose.dev.yml --env-file deploy/.env ps

# 4. Exercise it, then tear down (keeping data).
bash scripts/smoke.sh       # creates a policy + client, sends decisions
bash scripts/failover.sh    # stops the leader, verifies takeover + rejoin
docker compose -f deploy/compose.dev.yml --env-file deploy/.env down
```

The gateway is the only public port (`8080`); node debug ports and Prometheus
(`9090`) bind to loopback, and `/internal/*` and `/metrics` return `404` at the
gateway. The admin dashboard is at `http://localhost:8080/admin`.

## API example

Placeholders only — mint a real key in the dashboard. Full reference:
[`docs/api.md`](docs/api.md).

```bash
curl -i https://YOUR_DOMAIN/v1/decisions \
  -H 'Authorization: Bearer qlk_YOURPREFIX_your-secret' \
  -H 'Idempotency-Key: 01JEXAMPLE00000000000000000' \
  -H 'Content-Type: application/json' \
  --data '{"policy_id":"password_reset","subject":"demo_user_0042","cost":1}'
```

**Allowed — `200`:**

```json
{ "policy_id": "password_reset", "allowed": true, "cost": 1,
  "remaining_milli": 4000, "retry_after_ms": 0, "duplicate": false }
```

**Denied — `429`** (same shape with `"allowed": false`, a positive
`retry_after_ms`, and a `Retry-After` header in seconds).

On `503`/`504`, **retry with the same `Idempotency-Key`** — the commit may still
land, and reusing the key avoids double-charging. The `pkg/client` Go client does
this automatically (only when a key is supplied).

## Tests

```bash
make fmt-check     # gofmt is clean
go vet ./...
make test          # unit + integration
make race          # go test -race
make lint          # golangci-lint (.golangci.yml)
make vuln          # govulncheck
make secret-scan   # local secret pattern scan
make release-check # all of the above, in order, nothing suppressed

make smoke         # operator smoke test (needs a running cluster)
make failover      # leader-failover exercise (needs a running cluster)
make load BASE_URL=http://localhost:8080 API_KEY=qlk_... POLICY_ID=load  # k6
```

Distributed tests map to the invariants they protect (in `test/integration` and
`internal/testcluster`):

| Test | Invariant |
|------|-----------|
| isolated-leader-cannot-commit | no commit without a quorum |
| asymmetric-partition-still-commits | quorum via remaining links; minority returns `503` |
| follower catch-up after heal | replication repairs a lagging follower |
| old-leader conflict-suffix repair | conflicting suffixes are truncated to the leader's log |
| restart recovery / replay | committed-but-unapplied entries replay on restart |
| duplicate-RPCs-are-idempotent | repeated `AppendEntries` do not double-apply |
| hot-bucket concurrency | concurrent decisions on one subject never over-allow |

Performance is measured, not assumed: the load/failover methodology and a
baseline captured on a constrained dev box are in [`docs/demo.md`](docs/demo.md).
Numbers depend heavily on hardware (per-decision quorum commit + fsync).

## Metrics and operations

- **Readiness** (`/health/ready`) reports one of `shutting_down`, `starting`,
  `no_leader`, `no_quorum`, `lagging`, or `ready`; a load balancer should route
  only to `ready` nodes.
- **Key metrics**: `quorumlimiter_decisions_total{result}`,
  `quorumlimiter_elections_total`, proposal-to-apply latency, Raft role/term,
  commit/apply indexes and apply lag, and seconds since last quorum contact.
- **Initial Prometheus queries** (via an SSH tunnel to `127.0.0.1:9090`):
  `rate(quorumlimiter_decisions_total[1m])`,
  `changes(quorumlimiter_raft_term[5m])` (election churn),
  `quorumlimiter_apply_lag`.
- **Backups & logs**: encrypted per-volume backups and log inspection are in the
  [deployment runbook](docs/deployment.md); `deploy/alerts.yml` has starter alerts.

## Deployment

- **Single host** and **three-VM** instructions, plus verification, firewall,
  backup, restore rehearsal, image upgrade, and rollback:
  [`docs/deployment.md`](docs/deployment.md).
- Live demo URL: _not published_ (add here only if a maintained, safe endpoint
  exists).

## Design decisions

- **Go** — a small standard library, easy static binaries and containers, first
  class concurrency and the race detector, and `go test`/`go vet`/`govulncheck`
  in the toolchain.
- **bbolt** — an embedded, transactional key/value store; each node owns one file
  and the Raft log keeps them aligned, so there is no external database to run.
- **Server-rendered UI** — `html/template` with embedded assets and a strict CSP;
  no build step, no CDN, and no JavaScript is required for any mutation.
- **Integer token units** — milli-tokens make refill/spend deterministic and
  byte-identical across nodes; floats would drift.
- **Fixed three-node membership** — keeps the consensus core small and correct;
  dynamic membership (joint consensus) is deliberately out of scope.
- **No Redis** — a shared cache would reintroduce the very single-point,
  non-consensus state this project exists to avoid.

See the [ADRs](docs/adr/) for the fuller rationale.

## Limitations

- **Fixed membership** — exactly three nodes; no runtime add/remove.
- **No snapshots or log compaction** — the log grows unbounded over a very long
  lifetime.
- **One write leader** — all writes serialize through the leader; not horizontally
  write-scalable.
- **Educational node-to-node security** — a shared cluster bearer token on a
  private network, not mutual TLS.
- **Single-host demo** — three containers on one machine do not survive host
  failure; use the three-VM variant for infrastructure resilience.
- **Not formally verified** — tests are strong evidence, not a proof.

## Roadmap

Directional, matching the project's future-work analysis:

- **Snapshots + log compaction** (`InstallSnapshot`) — the largest operational gap.
- **Linearizable read path** (read-index / quorum-confirmed reads).
- **Mutual TLS** between nodes, with per-node certificates and rotation.
- **Clock-offset monitoring** and leadership refusal beyond a threshold.
- **Pre-vote and leadership transfer**; then **dynamic membership** (joint
  consensus).
- **Batched proposals / group commit**, measured rather than assumed.
- Product: API-key rotation, scoped admin roles, OpenAPI spec, tenant namespaces.
- Research: model the election/log rules in TLA+ and add property-based fault tests.

## Contributing

Run the full gate before opening a pull request:

```bash
make release-check
```

CI runs the same checks on every push and pull request (see
[`.github/workflows/ci.yml`](.github/workflows/ci.yml)). Keep commits focused and
describe the change; do not commit `deploy/.env`, database files, certificates,
or backups.

## License

[MIT](LICENSE) © 2026 Sarim Fazal.
