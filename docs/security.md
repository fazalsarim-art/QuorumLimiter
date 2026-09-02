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

## Limitations

Fixed three-node membership; a shared bearer token (not mTLS) between nodes;
single write leader; educational consensus not formally verified. See
`docs/architecture.md` and the ADRs.
