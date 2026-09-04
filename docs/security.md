# Security

This document describes QuorumLimiter's security boundaries and controls. It is
an educational implementation; see the limitations at the end.

## Secrets

Four secrets are supplied via environment and never logged, never placed in the
Raft log, and never returned in responses:

| Secret | Purpose |
|--------|---------|
| `QL_ADMIN_TOKEN`   | Admin sign-in credential (constant-time compared). |
| `QL_SESSION_KEY`   | HMAC key signing admin session cookies (shared by all nodes). |
| `QL_API_KEY_PEPPER`| HMAC key for client API-key digests. |
| `QL_CLUSTER_TOKEN` | Bearer credential for internal Raft RPCs. |

They live in an ignored `.env` locally; production generates fresh values on the
server. `.env.example` holds placeholders only.

## Client API keys

Keys have the form `qlk_<8-char prefix>_<secret>`. The raw key is generated on
the leader with `crypto/rand`, shown to the operator **once** at creation, and
never stored. Only the prefix (for lookup) and an `HMAC-SHA256(pepper, key)`
digest are replicated and persisted. Authentication recomputes the HMAC and
compares it in constant time; any failure — unknown, malformed, mismatched, or
revoked — returns a generic 401.

## Admin sessions and CSRF

Sign-in compares `QL_ADMIN_TOKEN` in constant time and issues an 8-hour signed
session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` in production) carrying
the issue time, expiry, and a random CSRF token. Every admin route verifies the
signature and expiry. State-changing requests additionally require a matching
`X-CSRF-Token` (double-submit against the signed session) and a same-origin
`Origin`/`Referer`. Failed sign-ins are rate-limited to five per source IP per
minute. Admin responses carry `Cache-Control: no-store` and security headers
(CSP, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: no-referrer`). A one-time raw key is returned only in the
immediate client-creation response.

## Internal RPCs

`/internal/raft/*` requires the cluster bearer token (constant-time), a
source-node allowlist, JSON content-type, a 64 KiB body cap, and strict
decoding. These routes must never be exposed through the public gateway (the
Caddy config blocks them, Phase 11). Node-to-node authorization today is a
shared bearer token on a private network; mutual TLS is future work.

## Request hardening

- 64 KiB body cap and strict JSON decoding (unknown fields rejected) on all
  JSON endpoints.
- Server read-header/read/write/idle timeouts bound slow clients.
- Redacted logs: no secrets, tokens, cookies, or full subjects. Audit records
  store subjects only as a short one-way hash.
- Metrics never use API keys, subjects, or client IDs as labels (Phase 10).

## Automated quality and security gates

`make release-check` runs the full gate in a fixed order and suppresses nothing:

| Step | Tool | What it catches |
|------|------|-----------------|
| `fmt-check` | `gofmt -l` | unformatted code |
| `vet` | `go vet` | suspicious constructs |
| `test` | `go test ./...` | functional regressions |
| `race` | `go test -race ./...` | data races |
| `lint` | `golangci-lint` (`.golangci.yml`) | errcheck, staticcheck, revive, misspell, unconvert, … |
| `vuln` | `govulncheck` | known CVEs in the std-lib and dependencies |
| `secret-scan` | `scripts/secret-scan.sh` | leaked credentials in tracked files |

`scripts/secret-scan.sh` greps only git-tracked files for private keys, cloud and
vendor tokens (AWS, GitHub, Slack, Google) and raw `qlk_` API keys, and fails the
build if `deploy/.env` is ever tracked. Findings are fixed, never allow-listed.

**Vulnerability remediation.** `govulncheck` flagged six Go standard-library
advisories fixed in **Go 1.26.6** (including `html/template` and `net/http`);
Phase 13 bumped the `toolchain` directive in `go.mod` from 1.26.5 to 1.26.6 and
the scan is now clean.

## Production deployment

The production Compose file (`deploy/compose.prod.yml`) and
[deployment runbook](deployment.md) harden the exposed surface:

- `QL_PRODUCTION=true` makes session/CSRF cookies `Secure` and sets an HTTPS
  public base URL; Caddy terminates TLS and is the only trusted proxy.
- **Caddy is the sole public service**, on 80/443, with automatic HTTPS and an
  added `Strict-Transport-Security` header. Node debug ports are not published,
  and Prometheus binds to loopback (reachable only via SSH tunnel).
- `/internal/*` and `/metrics` return `404` at the gateway, so consensus RPCs and
  metrics are never publicly reachable.
- The host firewall (`ufw`) allows only 22/80/443; the three-VM variant allows
  the Raft port only between the private peer addresses.
- Production secrets are generated fresh on the server, kept in a git-ignored
  `deploy/.env` (mode `600`), and passed only as environment references. Backups
  are encrypted with a key stored off-host.

## Limitations

Fixed three-node membership; a shared bearer token (not mTLS) between nodes;
single write leader; educational consensus not formally verified. See
`docs/architecture.md` and the ADRs.
