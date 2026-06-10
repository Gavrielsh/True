// k6 baseline load test for the True Engine money path.
//
//   k6 run loadtest/k6_bet_win.js
//
// Environment overrides:
//   BASE_URL        engine base URL          (default http://localhost:8080)
//   OPERATOR_CODE   HMAC operator code       (default PRAGMATIC)
//   OPERATOR_SECRET HMAC shared secret       (default dev-secret-pragmatic —
//                   must match OPERATOR_SECRETS on the engine)
//   TARGET_RPS      arrival rate             (default 10000)
//
// Scenario: constant arrival rate of TARGET_RPS for 60s across /api/v1/bet
// and /api/v1/win, executed by 200 pre-allocated VUs. Every request carries a
// unique operator_transaction_id (UUID v4), a fresh nonce, a current unix
// timestamp, and a correct HMAC-SHA256 hex signature over the RAW body.
//
// setup() provisions 1000 ACTIVE players and funds each with GC via
// /api/v1/store/purchase so bets never fail on INSUFFICIENT_FUNDS.

import http from 'k6/http';
import crypto from 'k6/crypto';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const OPERATOR_CODE = __ENV.OPERATOR_CODE || 'PRAGMATIC';
const OPERATOR_SECRET = __ENV.OPERATOR_SECRET || 'dev-secret-pragmatic';
const TARGET_RPS = parseInt(__ENV.TARGET_RPS || '10000', 10);
const PLAYER_COUNT = 1000;

export const options = {
  scenarios: {
    baseline: {
      executor: 'constant-arrival-rate',
      rate: TARGET_RPS,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 200,
      maxVUs: 200,
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<50', 'p(99)<200'],
    http_req_failed: ['rate<0.001'],
    checks: ['rate>0.999'],
  },
};

// uuidv4 without external jslib (offline-safe). Not cryptographically
// strong — these are load-test transaction ids, not secrets.
function uuidv4() {
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    const v = c === 'x' ? r : (r & 0x3) | 0x8;
    return v.toString(16);
  });
}

// signedPost sends body with the full security header set: HMAC-SHA256 hex
// signature over the RAW body, unix-seconds timestamp, single-use nonce.
function signedPost(path, body) {
  const signature = crypto.hmac('sha256', OPERATOR_SECRET, body, 'hex');
  return http.post(`${BASE_URL}${path}`, body, {
    headers: {
      'Content-Type': 'application/json',
      'X-Operator-Code': OPERATOR_CODE,
      'X-Signature': signature,
      'X-Timestamp': String(Math.floor(Date.now() / 1000)),
      'X-Nonce': uuidv4(),
    },
  });
}

// setup runs once: provision and fund the player pool.
export function setup() {
  const players = [];
  for (let i = 0; i < PLAYER_COUNT; i++) {
    const externalId = `k6-player-${i}-${uuidv4()}`;
    const createRes = signedPost(
      '/api/v1/player/create',
      JSON.stringify({ external_id: externalId, status: 'ACTIVE' }),
    );
    if (createRes.status !== 200 && createRes.status !== 201) {
      throw new Error(`player create ${i} failed: ${createRes.status} ${createRes.body}`);
    }
    const playerId = createRes.json('player_id');

    // Fund with a large GC package so 60s of 1.0000 bets can never drain it.
    const purchaseRes = signedPost(
      '/api/v1/store/purchase',
      JSON.stringify({
        operator_transaction_id: uuidv4(),
        player_id: playerId,
        gc_amount: '1000000.0000',
      }),
    );
    if (purchaseRes.status !== 200) {
      throw new Error(`player fund ${i} failed: ${purchaseRes.status} ${purchaseRes.body}`);
    }
    players.push(playerId);
  }
  return { players };
}

export default function (data) {
  const playerId = data.players[(Math.random() * data.players.length) | 0];
  const isBet = Math.random() < 0.5;

  const path = isBet ? '/api/v1/bet' : '/api/v1/win';
  const body = JSON.stringify({
    operator_transaction_id: uuidv4(),
    player_id: playerId,
    currency: 'GC',
    amount: isBet ? '1.0000' : '0.5000',
    game_id: 'K6-SLOT-1',
    round_id: uuidv4(),
  });

  const res = signedPost(path, body);

  check(res, {
    'status is 200': (r) => r.status === 200,
    // PROCESSED = fresh commit, GHOST_RECOVERED = idempotent replay after a
    // dropped cache write — both are financially correct successes.
    'body status is success': (r) =>
      r.body.includes('"status":"PROCESSED"') || r.body.includes('"status":"GHOST_RECOVERED"'),
  });
}

// Persist the run summary for CI artifacts / investor decks.
export function handleSummary(data) {
  return {
    'loadtest/results/summary.json': JSON.stringify(data, null, 2),
    stdout: textSummary(data),
  };
}

// Minimal text summary (avoids the external k6-summary jslib dependency).
function textSummary(data) {
  const m = data.metrics;
  const line = (name, metric) => {
    if (!metric || !metric.values) return `${name}: n/a`;
    const v = metric.values;
    if (v['p(95)'] !== undefined) {
      return `${name}: avg=${v.avg.toFixed(2)}ms p95=${v['p(95)'].toFixed(2)}ms p99=${(v['p(99)'] || 0).toFixed(2)}ms`;
    }
    if (v.rate !== undefined) return `${name}: rate=${(v.rate * 100).toFixed(3)}%`;
    if (v.count !== undefined) return `${name}: count=${v.count}`;
    return `${name}: ${JSON.stringify(v)}`;
  };
  return [
    '── True Engine k6 baseline ──',
    line('http_req_duration', m.http_req_duration),
    line('http_req_failed', m.http_req_failed),
    line('checks', m.checks),
    line('iterations', m.iterations),
    '',
  ].join('\n');
}
