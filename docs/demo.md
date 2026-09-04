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

## Cleanup

```bash
docker compose -f deploy/compose.dev.yml --env-file deploy/.env down     # keep data
docker compose -f deploy/compose.dev.yml --env-file deploy/.env down -v  # discard data
```
