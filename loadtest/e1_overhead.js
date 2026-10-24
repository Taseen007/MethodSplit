// E1: how much latency does the gateway add? Same GET /v1/balance request,
// once straight to the backend (bypassing the gateway's auth/rate-limit/
// cache/RYW pipeline) and once through the gateway.
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import { signedHeaders } from './lib/sign.js';

const DIRECT_URL = __ENV.DIRECT_URL || 'http://payments-write-1:9000';
const GATEWAY_URL = __ENV.GATEWAY_URL || 'http://gateway:8080';
const API_KEY = __ENV.API_KEY || 'mk_demo_a';
const SECRET = __ENV.SECRET;
const MERCHANT_ID = __ENV.MERCHANT_ID || 'merchant_demo_a';
const RATE = Number(__ENV.RATE || 20);
const DURATION = __ENV.DURATION || '20s';

export const directDuration = new Trend('direct_duration', true);
export const gatewayDuration = new Trend('gateway_duration', true);

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    direct: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: RATE,
      exec: 'directRequest',
    },
    gateway: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: RATE,
      startTime: DURATION, // sequential, so the two scenarios don't compete for CPU
      exec: 'gatewayRequest',
    },
  },
};

export function directRequest() {
  const res = http.get(`${DIRECT_URL}/v1/balance`, {
    headers: { 'X-Merchant-ID': MERCHANT_ID },
  });
  check(res, { 'direct status 200': (r) => r.status === 200 });
  directDuration.add(res.timings.duration);
}

export function gatewayRequest() {
  const headers = signedHeaders(API_KEY, SECRET, 'GET', '/v1/balance', '');
  const res = http.get(`${GATEWAY_URL}/v1/balance`, { headers });
  check(res, { 'gateway status 200': (r) => r.status === 200 });
  gatewayDuration.add(res.timings.duration);
}

export function handleSummary(data) {
  const d = data.metrics.direct_duration.values;
  const g = data.metrics.gateway_duration.values;
  const fmt = (m) => `p50=${m['med'].toFixed(2)}ms p95=${m['p(95)'].toFixed(2)}ms p99=${m['p(99)'].toFixed(2)}ms avg=${m.avg.toFixed(2)}ms`;
  const summary = [
    '=== E1: Gateway overhead (GET /v1/balance) ===',
    `direct (backend only):   ${fmt(d)}`,
    `gateway (full pipeline): ${fmt(g)}`,
    `overhead (p95 gateway - p95 direct) = ${(g['p(95)'] - d['p(95)']).toFixed(2)}ms`,
  ].join('\n');
  return { stdout: summary + '\n' };
}
