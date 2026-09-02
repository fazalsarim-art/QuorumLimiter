# API

Public JSON APIs live under `/v1`. The private Raft endpoints under
`/internal/raft` (Phase 5) are cluster-only and must never be exposed publicly.

## Conventions

- All bodies are UTF-8 JSON. Request bodies are capped at 64 KiB and unknown
  fields are rejected.
- Every response carries an `X-Request-ID` header for correlation. Clients may
  supply their own via the same header; otherwise the server generates one.
- Errors use a stable envelope:

  ```json
  { "error": { "code": "invalid_api_key", "message": "…", "request_id": "req_…" } }
  ```

## POST /v1/decisions

Consume tokens for a subject under a policy. Callable on any node: a follower
forwards the request to the current leader over the private network. If no
leader is known, it returns `503` with `Retry-After`.

### Request

Headers:

- `Authorization: Bearer qlk_<prefix>_<secret>` — client API key.
- `Idempotency-Key: <16–128 URL-safe chars>` — identifies the logical operation.
- `Content-Type: application/json`

Body:

```json
{ "policy_id": "password_reset", "subject": "customer_4821", "cost": 1 }
```

`policy_id` matches `^[a-z][a-z0-9_]{2,47}$`; `subject` is 1–128 chars matching
`^[A-Za-z0-9._:@/-]+$`; `cost` is a positive integer not exceeding the policy's
maximum.

### Allowed — 200

```json
{
  "request_id": "idem-000000000001",
  "policy_id": "password_reset",
  "allowed": true,
  "cost": 1,
  "remaining_milli": 4000,
  "retry_after_ms": 0,
  "observed_at": "2026-07-29T08:00:00Z",
  "leader_term": 18,
  "log_index": 843,
  "duplicate": false
}
```

`remaining_milli` is the balance in milli-tokens (1000 = one token). A replay of
the same idempotency key returns the original decision with `duplicate: true`
and no additional charge.

### Denied — 429

Same body shape with `allowed: false`, a positive `retry_after_ms`, and an
integer `Retry-After` header (seconds).

### Errors

| Status | Code                     | When |
|--------|--------------------------|------|
| 400    | `invalid_json`           | Malformed JSON or unknown field |
| 401    | `invalid_api_key`        | Missing, malformed, unknown, or revoked key |
| 403    | `forbidden_policy`       | Key is not allowed to use the policy |
| 404    | `policy_not_found`       | Policy does not exist or is inactive |
| 409    | `idempotency_conflict`   | Same key reused with different content |
| 413    | `request_too_large`      | Body exceeds 64 KiB |
| 415    | `unsupported_media_type` | Content-Type is not JSON |
| 422    | `validation_failed`      | Invalid policy id, subject, cost, or idempotency key |
| 429    | `rate_limited`           | Denied by the token bucket (decision body, not envelope) |
| 503    | `leader_unavailable`     | No leader or no quorum — retry (has `Retry-After`) |
| 504    | `proposal_timeout`       | Wait ended before a known result |

A `504` caller must **retry with the same `Idempotency-Key`**: the command may
still commit, and reusing the key avoids charging twice.

## Go client

`pkg/client` is a dependency-free client. `Decide` retries `503`/`504` and
network errors **only when an idempotency key is supplied**, since retrying
without one could double-charge.

```go
c := client.New("http://localhost:8080", "qlk_…")
resp, err := c.Decide(ctx, client.DecideRequest{PolicyID: "password_reset", Subject: "customer_4821", Cost: 1}, "idem-000000000001")
```
