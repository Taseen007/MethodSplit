// E2 (the core claim): does the read/write split protect writes? Run the
// same GET flood + steady POST workload twice — once against a gateway
// with split_enabled: true, once with split_enabled: false — and compare
// POST p95 and error rate. Point GATEWAY_URL at whichever config is
// currently deployed; run this script twice, once per config.
import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate } from 'k6/metrics';
import { signedHeaders } from './lib/sign.js';

const GATEWAY_URL = __ENV.GATEWAY_URL || 'http://gateway:8080';
const API_KEY = __ENV.API_KEY || 'mk_demo_a';
const SECRET = __ENV.SECRET;
const GET_RATE = Number(__ENV.GET_RATE || 150);
const POST_RATE = Number(__ENV.POST_RATE || 10);
const DURATION = __ENV.DURATION || '20s';

export const postDuration = new Trend('post_duration', true);
export const postErrors = new Rate('post_errors');
export const getErrors = new Rate('get_errors');

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    get_flood: {
      executor: 'constant-arrival-rate',
      rate: GET_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: GET_RATE,
      maxVUs: GET_RATE * 2,
      exec: 'getRequest',
    },
    steady_posts: {
      executor: 'constant-arrival-rate',
      rate: POST_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: POST_RATE,
      maxVUs: POST_RATE * 4,
      exec: 'postRequest',
    },
  },
};

export function getRequest() {
  const headers = signedHeaders(API_KEY, SECRET, 'GET', '/v1/payments', '');
  const res = http.get(`${GATEWAY_URL}/v1/payments`, { headers });
  getErrors.add(res.status !== 200);
}

export function postRequest() {
  const body = JSON.stringify({ amount_minor: 100, currency: 'BDT' });
  const idemKey = `e2-${__VU}-${__ITER}-${Date.now()}`;
  const headers = signedHeaders(API_KEY, SECRET, 'POST', '/v1/payments', body);
  headers['Idempotency-Key'] = idemKey;
  const res = http.post(`${GATEWAY_URL}/v1/payments`, body, { headers });
  const ok = check(res, { 'post status 201': (r) => r.status === 201 });
  postDuration.add(res.timings.duration);
  postErrors.add(!ok);
}

export function handleSummary(data) {
  const d = data.metrics.post_duration.values;
  const errRate = data.metrics.post_errors.values.rate;
  const getErrRate = data.metrics.get_errors.values.rate;
  const summary = [
    '=== E2: split_enabled workload (GET flood + steady POSTs) ===',
    `POST p50=${d['med'].toFixed(2)}ms p95=${d['p(95)'].toFixed(2)}ms p99=${d['p(99)'].toFixed(2)}ms avg=${d.avg.toFixed(2)}ms`,
    `POST error rate = ${(errRate * 100).toFixed(2)}%`,
    `GET error rate = ${(getErrRate * 100).toFixed(2)}%`,
  ].join('\n');
  return { stdout: summary + '\n' };
}
