# Demonstration

A two-minute walkthrough of QuorumLimiter's core behavior and failure handling,
plus the automated `smoke` and `failover` scripts.

## Prerequisites

- Docker Engine and Compose.
- `deploy/.env` with the four shared secrets (`cp deploy/.env.example deploy/.env`
  and fill them in).

## Start the cluster

```bash
docker compose -f deploy/compose.dev.yml --env-file deploy/.env up -d --build
docker compose -f deploy/compose.dev.yml --env-file deploy/.env ps
```

All three nodes should report `healthy`, with Caddy on `:8080` and Prometheus on
`127.0.0.1:9090`.

## Two-minute script

1. **Show the leader and matching indexes.** With the cluster token, query each
   node's private status (loopback debug ports 18081–18083):

   ```bash
   for p in 18081 18082 18083; do
     curl -fsS -H "Authorization: Bearer $QL_CLUSTER_TOKEN" \
       http://localhost:$p/internal/raft/status | grep -oE '"role":"[a-z]+"|"commit_index":[0-9]+'
   done
   ```

   Exactly one node is `leader`; all commit indexes converge.

2. **Create a policy and client, then decide.** Run the smoke script — it signs
   in, creates a 3-token policy and a client, and sends four decisions through
   the gateway:

   ```bash
   bash scripts/smoke.sh
   ```

   Expect `allowed=3 denied=1` and `SMOKE OK` (the fourth request exhausts the
   bucket → `429`).

3. **Idempotency.** Repeating a decision with the same `Idempotency-Key` returns
   the original result with `"duplicate": true` and does not charge twice.

4. **Failover.** Stop the leader, watch a new one take over, keep serving, then
   restart the old node and watch it catch up:

   ```bash
   bash scripts/failover.sh
   ```

   Expect a new leader within ~1–2 seconds, a successful post-failover decision,
   and the old node rejoining as a `follower` (`FAILOVER OK`).

5. **Metrics.** Prometheus scrapes every node; try graphing
   `rate(quorumlimiter_decisions_total[1m])` at `http://localhost:9090`.

## Interview talking points

- Why a decision is returned only after quorum **commit and apply**.
- Why token math is integer-only and the timestamp lives inside the command.
- Why the current-term commit rule protects safety.
- What happens during a partition, and why a 504 caller retries with the same
  idempotency key.
- Where this educational implementation stops (see `docs/architecture.md`).

## Load, performance, and failover-under-load

`scripts/load.js` is a [k6](https://k6.io) test with two concurrent scenarios —
`spread` (each request targets a distinct subject, so buckets rarely drain) and
`hot` (a few VUs hammer one subject). Responses are counted separately as
allowed / denied / 503 / unexpected, and it enforces two thresholds: **unexpected
< 1%** and **decision p95 < 250 ms** (a hardware-dependent SLO). It requires
`BASE_URL`, `API_KEY` and `POLICY_ID` from the environment — no keys are baked
into the repo. Mint a key via the admin API, then:

```bash
k6 run -e BASE_URL=http://localhost:8080 -e API_KEY=qlk_... -e POLICY_ID=load scripts/load.js
```

For a leader-failure measurement, use a longer run (e.g. `-e DURATION=40s`) and
stop the leader at roughly the midpoint (`make failover`, or
`docker compose -f deploy/compose.dev.yml --env-file deploy/.env stop nodeN`); the
`ql_unavailable_503` counter captures the outage window, and recovery is the
return of `ql_allowed` and latency to baseline.

### Recorded baseline

| | |
|---|---|
| Toolchain | Go 1.26.6; Grafana k6 2.2.0 |
| Host | WSL2 Ubuntu on Windows 11; three node containers each capped at **1.0 CPU / 256 MB** (`deploy/compose.dev.yml`), behind the Caddy gateway |
| Workload | `spread` 25 VUs + `hot` 5 VUs, 60 s, 0.1 s think-time, cost 1; policy capacity 200, refill 200/s |

Steady state (fresh cluster):

- **11,869 decisions at ~197 req/s**; 0 denied, 0×503, **0 unexpected (0.00%)**.
- Decision latency: avg 51 ms, **p95 66.7 ms**, max 184 ms — comfortably under the
  250 ms SLO. Both thresholds pass.

Latency is dominated by per-decision **quorum commit + fsync**: every decision is
a replicated Raft log entry, and apply runs in the single event loop, so latency
rises with offered concurrency. On constrained hardware a closed-loop generator
with no think-time can drive the cluster into contention collapse (all latency
budget consumed, heartbeats missed → election churn → a 503 storm), which is why
`load.js` includes a small think-time. Absolute numbers depend heavily on CPU
limits and disk fsync latency.

Leader failure at mid-run (40 s run, leader stopped at t ≈ 15 s):

- New leader elected in **~1.6 s**.
- **211 requests received 503** (with `Retry-After`) during the election window;
  `ql_unexpected` stayed at **0.01% (< 1%)**.
- `ql_allowed` and latency returned to baseline immediately afterward; the stopped
  node rejoined as a follower.
- Decision p95 for that run was 354 ms — the failover run is judged on recovery
  time and error rate, not on the steady-state latency SLO.

## Quality gate

One reproducible command runs every check in a fixed order and suppresses nothing:

```bash
make release-check   # fmt-check, vet, test, race, lint, vuln, secret-scan
```

- **golangci-lint** (`.golangci.yml`) — the standard linters plus
  revive / misspell / unconvert.
- **govulncheck** — the Go vulnerability database. Phase 13 bumped the toolchain
  to Go 1.26.6 to clear six standard-library advisories.
- **secret-scan** (`scripts/secret-scan.sh`) — greps tracked files for private
  keys, cloud/vendor tokens and raw API keys; real secrets live only in the
  gitignored `deploy/.env`.

See [`docs/security.md`](security.md) for the full security posture.

## Cleanup

```bash
docker compose -f deploy/compose.dev.yml --env-file deploy/.env down     # keep data
docker compose -f deploy/compose.dev.yml --env-file deploy/.env down -v  # discard data
```
