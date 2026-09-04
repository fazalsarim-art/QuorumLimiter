// load.js — k6 load & performance test for QuorumLimiter's public decision API.
//
// It runs two scenarios concurrently:
//   * spread — every request targets a distinct subject, so buckets rarely drain
//     and the cluster mostly ALLOWS. Exercises throughput, forwarding and
//     replication under a wide fan-out of independent keys.
//   * hot    — a small pool of VUs hammer ONE subject, which drains and then
//     DENIES. Exercises the deny path and hot-key contention on a single bucket.
//
// Responses are classified into separate counters so allow/deny/unavailable are
// never conflated with real failures:
//   200 -> allowed          (expected)
//   429 -> denied           (expected — bucket empty, Retry-After set)
//   503 -> unavailable      (expected briefly during a leader failover)
//   else -> unexpected      (a real problem — gated to < 1%)
//
// Required environment variables (the script refuses to run without them, and no
// credentials are ever embedded in the repo):
//   BASE_URL   e.g. http://localhost:8080  (the gateway, not a node's internal port)
//   API_KEY    a raw client key (qlk_...) minted via the admin API
//   POLICY_ID  the policy the API_KEY is allowed to use
//
// Optional knobs:
//   VUS            steady virtual users for the spread scenario (default 25)
//   DURATION       run length (default 60s)
//   SUBJECT_SPACE  number of distinct subjects in the spread scenario (default 5000)
//   HOT_SUBJECT    subject name for the hot scenario (default "hot-subject")
//
// Baseline run (start at 25 VUs, then raise DURATION/VUS carefully once stable):
//   k6 run -e BASE_URL=http://localhost:8080 -e API_KEY=qlk_xxx -e POLICY_ID=load scripts/load.js
//
// Leader-failure measurement: run with a longer DURATION and, at roughly the
// midpoint, stop the current leader (`make failover`, or `docker compose ... stop nodeN`).
// The ql_unavailable_503 counter captures the failover window; recovery is the
// return of ql_allowed and latency to baseline. Evaluate that run on recovery,
// not on the steady-state p95 threshold below.

import http from 'k6/http';
import { sleep } from 'k6';
import { Counter, Trend, Rate } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL;
const API_KEY = __ENV.API_KEY;
const POLICY_ID = __ENV.POLICY_ID;
if (!BASE_URL || !API_KEY || !POLICY_ID) {
  throw new Error('BASE_URL, API_KEY and POLICY_ID environment variables are required');
}

const VUS = Number(__ENV.VUS || 25);
const DURATION = __ENV.DURATION || '60s';
const SUBJECT_SPACE = Number(__ENV.SUBJECT_SPACE || 5000);
const HOT_SUBJECT = __ENV.HOT_SUBJECT || 'hot-subject';
// Per-iteration think-time (seconds). Because these are closed-model (constant-vus)
// scenarios, a VU with zero think-time re-fires the instant a response returns —
// so when responses fast-fail (e.g. 503 during a leaderless window) the VUs turn
// into a thundering herd that can prevent a small cluster from recovering. A
// small think-time bounds the offered rate and keeps the load realistic.
const THINK = Number(__ENV.THINK || 0.1);

// Idempotency keys must be 16-128 chars of [A-Za-z0-9._~-]. RUN is a per-VU
// nonce so keys stay unique across re-runs (decision records persist ~24h, and a
// reused key with different content would return 409). Combined with scenario +
// __VU + __ITER, every request gets a distinct key comfortably over 16 chars.
const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e9).toString(36)}`;

const allowed = new Counter('ql_allowed');
const denied = new Counter('ql_denied');
const unavailable = new Counter('ql_unavailable_503');
const unexpected = new Counter('ql_unexpected');
const unexpectedRate = new Rate('ql_unexpected_rate');
const decideLatency = new Trend('ql_decide_latency', true);

export const options = {
  scenarios: {
    spread: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      exec: 'spread',
      tags: { scenario: 'spread' },
    },
    hot: {
      executor: 'constant-vus',
      vus: Math.max(1, Math.floor(VUS / 5)),
      duration: DURATION,
      exec: 'hot',
      tags: { scenario: 'hot' },
    },
  },
  thresholds: {
    // Unexpected (non-200/429/503) responses must stay under 1%.
    ql_unexpected_rate: ['rate<0.01'],
    // Steady-state decision latency budget on the documented dev machine.
    // Latency is hardware- and network-dependent; treat as a relative SLO.
    ql_decide_latency: ['p(95)<250'],
  },
};

function decide(subject, key) {
  const res = http.post(
    `${BASE_URL}/v1/decisions`,
    JSON.stringify({ policy_id: POLICY_ID, subject: subject, cost: 1 }),
    {
      headers: {
        'Content-Type': 'application/json',
        'Authorization': `Bearer ${API_KEY}`,
        'Idempotency-Key': key,
      },
      tags: { name: 'decide' },
    },
  );
  decideLatency.add(res.timings.duration);
  switch (res.status) {
    case 200: allowed.add(1); unexpectedRate.add(false); break;
    case 429: denied.add(1); unexpectedRate.add(false); break;
    case 503: unavailable.add(1); unexpectedRate.add(false); break;
    default: unexpected.add(1); unexpectedRate.add(true); break;
  }
  return res;
}

export function spread() {
  const n = Math.floor(Math.random() * SUBJECT_SPACE);
  decide(`sub-${n}`, `spread-${RUN}-${__VU}-${__ITER}`);
  sleep(THINK);
}

export function hot() {
  decide(HOT_SUBJECT, `hot-${RUN}-${__VU}-${__ITER}`);
  sleep(THINK);
}
