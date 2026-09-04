# QuorumLimiter

A three-node distributed token-bucket rate limiter whose state changes are ordered by a
Raft-lite consensus implementation written in Go.

> **Status:** early setup. This README is a placeholder and will be expanded during the
> documentation phase. See `QuorumLimiter.pdf` for the complete build guide.

## What it is

QuorumLimiter decides whether a caller may perform an action (for example, "may this user
send another password-reset email under a 5-per-minute policy?"). Every state-changing
decision is committed to a replicated log by a majority of nodes before it is applied and
returned, so three independent servers can never collectively over-allow.

## Quick start (Docker Compose)

Runs a three-node cluster behind a Caddy gateway, plus Prometheus.

```bash
# 1. Provide shared secrets for the cluster.
cp deploy/.env.example deploy/.env
# then edit deploy/.env and set the four secrets (see the file for openssl commands)

# 2. Build the image and start the cluster.
docker compose -f deploy/compose.dev.yml --env-file deploy/.env up -d --build

# 3. Check health (through the gateway) and find the leader.
curl -fsS http://localhost:8080/health/live
docker compose -f deploy/compose.dev.yml --env-file deploy/.env ps

# 4. Stop the cluster (keep data).
docker compose -f deploy/compose.dev.yml --env-file deploy/.env down
```

The gateway is the only public port (`8080`). Node debug ports and Prometheus
(`9090`) bind to loopback, and `/internal/*` and `/metrics` are blocked at the
gateway. The admin dashboard is at `http://localhost:8080/admin`. See
`docs/api.md` for the decision API and `docs/architecture.md` for the design.

## Development

```bash
make release-check   # fmt-check, vet, test, race, lint, vuln, secret-scan — one gate, nothing suppressed
make load BASE_URL=http://localhost:8080 API_KEY=qlk_... POLICY_ID=load   # k6 load test
```

`make help` lists all targets. Performance results and the load/failover
methodology are in [`docs/demo.md`](docs/demo.md); the security posture and gate
details are in [`docs/security.md`](docs/security.md).

## Planned stack

Go 1.26.6 · bbolt · Prometheus 3.13.1 · Docker + Compose · Caddy 2.11.4

> **Note:** this is an educational Raft implementation with fixed three-node membership. It
> demonstrates consensus mechanics and failure handling; it is not a substitute for a
> formally verified production consensus library.
