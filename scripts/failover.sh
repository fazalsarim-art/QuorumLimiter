#!/usr/bin/env bash
#
# failover.sh — demonstrate leader failover against the running Docker cluster:
# seed a policy/client, stop the current leader, wait for a new leader, send a
# decision through the gateway, then restart the old node and confirm it rejoins
# as a follower and catches up.
set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
ENV_FILE="${ENV_FILE:-deploy/.env}"
COMPOSE="docker compose -f deploy/compose.dev.yml --env-file ${ENV_FILE}"

declare -A PORT=([node1]=18081 [node2]=18082 [node3]=18083)

fail() { echo "failover: $*" >&2; exit 1; }

command -v curl >/dev/null || fail "curl is required"
command -v docker >/dev/null || fail "docker is required"
[ -f "$ENV_FILE" ] || fail "missing $ENV_FILE"
set -a; . "$ENV_FILE"; set +a
: "${QL_ADMIN_TOKEN:?}"; : "${QL_CLUSTER_TOKEN:?}"
case "$QL_ADMIN_TOKEN" in *REPLACE*) fail "refusing to run with placeholder secrets" ;; esac

role_of() {
  curl -fsS -H "Authorization: Bearer $QL_CLUSTER_TOKEN" \
    "http://localhost:${PORT[$1]}/internal/raft/status" 2>/dev/null | grep -oE '"role":"[a-z]+"' | cut -d'"' -f4 || true
}

find_leader() {
  for n in node1 node2 node3; do
    [ "$(role_of "$n")" = "leader" ] && { echo "$n"; return 0; }
  done
  return 1
}

wait_leader_excluding() { # $1 excluded node -> new leader (or fail)
  local excl="$1" deadline=$(( $(date +%s) + 15 )) n
  while [ "$(date +%s)" -lt "$deadline" ]; do
    for n in node1 node2 node3; do
      [ "$n" = "$excl" ] && continue
      [ "$(role_of "$n")" = "leader" ] && { echo "$n"; return 0; }
    done
    sleep 0.3
  done
  return 1
}

leader="$(find_leader)" || fail "no leader found"
echo "current leader: $leader"
LEADER_URL="http://localhost:${PORT[$leader]}"

JAR="$(mktemp)"; trap 'rm -f "$JAR"' EXIT
[ "$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" --data "admin_token=$QL_ADMIN_TOKEN" "$LEADER_URL/admin/login")" = "303" ] || fail "sign-in failed"
CSRF="$(grep ql_csrf "$JAR" | awk '{print $NF}')"
POLICY="failover_$(date +%s)"
curl -fsS -o /dev/null -b "$JAR" -H "X-CSRF-Token: $CSRF" -H "Origin: $LEADER_URL" -H "Content-Type: application/json" \
  --data "{\"id\":\"$POLICY\",\"name\":\"Failover demo\",\"capacity_tokens\":100,\"refill_tokens\":1,\"refill_interval_ms\":3600000,\"max_cost_tokens\":1,\"active\":true}" \
  "$LEADER_URL/api/admin/policies" || fail "create policy failed"
KEY="$(curl -fsS -b "$JAR" -H "X-CSRF-Token: $CSRF" -H "Origin: $LEADER_URL" -H "Content-Type: application/json" \
  --data "{\"name\":\"failover-client\",\"allowed_policy_ids\":[\"$POLICY\"]}" "$LEADER_URL/api/admin/clients" | grep -oE '"api_key":"[^"]+"' | cut -d'"' -f4)"
[ -n "$KEY" ] || fail "create client failed"
echo "seeded policy + client"

decide() { # $1 idempotency-suffix -> http code
  curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/v1/decisions" \
    -H "Authorization: Bearer $KEY" -H "Idempotency-Key: fo-$POLICY-$1" -H "Content-Type: application/json" \
    --data "{\"policy_id\":\"$POLICY\",\"subject\":\"s\",\"cost\":1}"
}
echo "pre-failover decision via gateway: HTTP $(decide pre)"

echo "stopping leader $leader ..."
start="$(date +%s.%N)"
$COMPOSE stop "$leader" >/dev/null 2>&1 || fail "could not stop $leader"
newleader="$(wait_leader_excluding "$leader")" || fail "no new leader elected within timeout"
elapsed="$(awk "BEGIN{printf \"%.1f\", $(date +%s.%N)-$start}")"
echo "new leader: $newleader (elected in ~${elapsed}s)"

code=""
for i in 1 2 3 4 5 6; do
  code="$(decide "post$i")"
  [ "$code" = "200" ] && break
  sleep 0.5
done
echo "post-failover decision via gateway: HTTP $code"
[ "$code" = "200" ] || fail "no successful decision after failover"

echo "restarting old node $leader ..."
$COMPOSE start "$leader" >/dev/null 2>&1 || fail "could not start $leader"
deadline=$(( $(date +%s) + 25 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if [ "$(role_of "$leader")" = "follower" ] &&
     [ "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${PORT[$leader]}/health/ready")" = "200" ]; then
    echo "$leader rejoined as follower and is ready (caught up)"
    echo "FAILOVER OK"
    exit 0
  fi
  sleep 0.5
done
fail "old node did not rejoin cleanly"
