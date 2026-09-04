#!/usr/bin/env bash
#
# smoke.sh — end-to-end check against a running cluster: sign in, create a unique
# policy and client, then send allowed and denied decisions through the gateway.
#
# Admin mutations target the leader's (loopback) debug port to avoid follower
# 503s; decisions go through the gateway to exercise leader forwarding.
set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
NODE_PORTS="${NODE_PORTS:-18081 18082 18083}"
ENV_FILE="${ENV_FILE:-deploy/.env}"

fail() { echo "smoke: $*" >&2; exit 1; }

command -v curl >/dev/null || fail "curl is required"
[ -f "$ENV_FILE" ] || fail "missing $ENV_FILE (copy deploy/.env.example and set secrets)"
set -a; . "$ENV_FILE"; set +a
: "${QL_ADMIN_TOKEN:?admin token not set}"
: "${QL_CLUSTER_TOKEN:?cluster token not set}"
case "$QL_ADMIN_TOKEN" in *REPLACE*) fail "refusing to run with placeholder secrets" ;; esac

role_of() { # $1 port -> role
  curl -fsS -H "Authorization: Bearer $QL_CLUSTER_TOKEN" \
    "http://localhost:$1/internal/raft/status" 2>/dev/null | grep -oE '"role":"[a-z]+"' | cut -d'"' -f4 || true
}

leader_port=""
for p in $NODE_PORTS; do
  [ "$(role_of "$p")" = "leader" ] && { leader_port="$p"; break; }
done
[ -n "$leader_port" ] || fail "no leader found on ports: $NODE_PORTS"
LEADER="http://localhost:$leader_port"
echo "leader debug port: $LEADER"

JAR="$(mktemp)"; trap 'rm -f "$JAR"' EXIT

code="$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" \
  --data "admin_token=$QL_ADMIN_TOKEN" "$LEADER/admin/login")"
[ "$code" = "303" ] || fail "admin sign-in failed (HTTP $code)"
CSRF="$(grep ql_csrf "$JAR" | awk '{print $NF}')"
[ -n "$CSRF" ] || fail "no CSRF token after sign-in"
echo "signed in"

POLICY="smoke_$(date +%s)"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" \
  -H "X-CSRF-Token: $CSRF" -H "Origin: $LEADER" -H "Content-Type: application/json" \
  --data "{\"id\":\"$POLICY\",\"name\":\"Smoke $POLICY\",\"capacity_tokens\":3,\"refill_tokens\":1,\"refill_interval_ms\":3600000,\"max_cost_tokens\":1,\"active\":true}" \
  "$LEADER/api/admin/policies")"
[ "$code" = "201" ] || fail "create policy failed (HTTP $code)"
echo "created policy $POLICY (capacity 3)"

resp="$(curl -s -b "$JAR" -H "X-CSRF-Token: $CSRF" -H "Origin: $LEADER" -H "Content-Type: application/json" \
  --data "{\"name\":\"smoke-client\",\"allowed_policy_ids\":[\"$POLICY\"]}" \
  "$LEADER/api/admin/clients")"
KEY="$(printf '%s' "$resp" | grep -oE '"api_key":"[^"]+"' | cut -d'"' -f4)"
[ -n "$KEY" ] || fail "create client failed: $resp"
echo "created client and received a one-time API key"

allowed=0; denied=0
for i in 1 2 3 4; do
  code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/v1/decisions" \
    -H "Authorization: Bearer $KEY" -H "Idempotency-Key: smoke-$POLICY-$i" \
    -H "Content-Type: application/json" \
    --data "{\"policy_id\":\"$POLICY\",\"subject\":\"demo_user_0042\",\"cost\":1}")"
  case "$code" in
    200) allowed=$((allowed + 1)) ;;
    429) denied=$((denied + 1)) ;;
    *)   fail "unexpected decision status HTTP $code" ;;
  esac
done
echo "decisions via gateway: allowed=$allowed denied=$denied"
[ "$allowed" -eq 3 ] && [ "$denied" -eq 1 ] || fail "expected 3 allowed / 1 denied for capacity 3"

echo "SMOKE OK"
