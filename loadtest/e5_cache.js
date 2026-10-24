// E5: how much does caching help? Repeated GET /v1/payments from a small
// pool of merchants (so cache keys repeat and a real hit ratio emerges),
// reporting p95 and the hit ratio via the X-Cache response header. Run
// this script twice: once against a gateway config with the route's
// cache_ttl > 0 ("on"), once with cache_ttl: 0 ("off").
import http from 'k6/http';
import { Trend, Rate } from 'k6/metrics';
import { signedHeaders } from './lib/sign.js';

const GATEWAY_URL = __ENV.GATEWAY_URL || 'http://gateway:8080';
const SECRET = __ENV.SECRET;
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || '20s';
// A small, fixed pool of API keys so requests repeat the same cache keys.
const API_KEYS = (__ENV.API_KEYS || 'mk_demo_a,mk_demo_b').split(',');

export const readDuration = new Trend('read_duration', true);
export const cacheHitRate = new Rate('cache_hit');

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    reads: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: RATE,
      maxVUs: RATE * 2,
    },
  },
};

export default function () {
  const apiKey = API_KEYS[__VU % API_KEYS.length];
  const headers = signedHeaders(apiKey, SECRET, 'GET', '/v1/payments', '');
  const res = http.get(`${GATEWAY_URL}/v1/payments`, { headers });
  readDuration.add(res.timings.duration);
  cacheHitRate.add(res.headers['X-Cache'] === 'HIT');
}

export function handleSummary(data) {
  const d = data.metrics.read_duration.values;
  const hitRate = data.metrics.cache_hit.values.rate;
  const summary = [
    '=== E5: cache hit ratio and latency (GET /v1/payments) ===',
    `p50=${d['med'].toFixed(2)}ms p95=${d['p(95)'].toFixed(2)}ms p99=${d['p(99)'].toFixed(2)}ms avg=${d.avg.toFixed(2)}ms`,
    `cache hit ratio = ${(hitRate * 100).toFixed(2)}%`,
  ].join('\n');
  return { stdout: summary + '\n' };
}
