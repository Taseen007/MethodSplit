import crypto from 'k6/crypto';

// Mirrors internal/middleware.CanonicalString / Sign (ARCHITECTURE.md §6):
// canonical = METHOD \n PATH_WITH_QUERY \n TIMESTAMP \n hex(SHA256(BODY))
export function signedHeaders(apiKey, secret, method, pathWithQuery, body) {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const bodyHash = crypto.sha256(body || '', 'hex');
  const canonical = `${method}\n${pathWithQuery}\n${timestamp}\n${bodyHash}`;
  const signature = crypto.hmac('sha256', secret, canonical, 'hex');
  return {
    'Content-Type': 'application/json',
    'X-Api-Key': apiKey,
    'X-Timestamp': timestamp,
    'X-Signature': signature,
  };
}
