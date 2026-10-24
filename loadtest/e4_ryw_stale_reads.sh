#!/bin/sh
# E4: does read-your-writes prevent stale reads? For N iterations: create a
# payment, then immediately list — once through the gateway (RYW active)
# and once straight against a read replica (no RYW) — and count how often
# the new payment is missing from that list (stale).
set -e

GW=${GW:-http://gateway:8080}
READ_DIRECT=${READ_DIRECT:-http://payments-read-1:9000}
API_KEY=${API_KEY:-mk_demo_a}
SECRET=${SECRET:-sk_demo_a_do_not_use_in_prod}
MERCHANT_ID=${MERCHANT_ID:-merchant_demo_a}
N=${N:-200}

sign() {
  method=$1; path=$2; body=$3
  ts=$(date +%s)
  body_hash=$(printf '%s' "$body" | openssl dgst -sha256 -r | cut -d' ' -f1)
  canonical=$(printf '%s\n%s\n%s\n%s' "$method" "$path" "$ts" "$body_hash")
  sig=$(printf '%s' "$canonical" | openssl dgst -sha256 -hmac "$SECRET" -r | cut -d' ' -f1)
  printf '%s|%s' "$ts" "$sig"
}

stale_with_ryw=0
stale_without_ryw=0
max_lag=0

for i in $(seq 1 "$N"); do
  body="{\"amount_minor\":$((100 + i)),\"currency\":\"BDT\"}"
  idem="e4-$i-$(date +%s%N)"
  ts_sig=$(sign POST /v1/payments "$body")
  ts=${ts_sig%%|*}
  sig=${ts_sig##*|}

  create_resp=$(curl -s -X POST "$GW/v1/payments" \
    -H "X-Api-Key: $API_KEY" -H "X-Timestamp: $ts" -H "X-Signature: $sig" \
    -H "Idempotency-Key: $idem" -H "Content-Type: application/json" -d "$body")
  id=$(printf '%s' "$create_resp" | grep -o '"id":"[^"]*' | head -1 | cut -d'"' -f4)
  if [ -z "$id" ]; then
    echo "create failed on iteration $i: $create_resp"
    continue
  fi

  ts_sig2=$(sign GET /v1/payments?limit=5 "")
  ts2=${ts_sig2%%|*}
  sig2=${ts_sig2##*|}
  list_full=$(curl -s -D - "$GW/v1/payments?limit=5" \
    -H "X-Api-Key: $API_KEY" -H "X-Timestamp: $ts2" -H "X-Signature: $sig2")
  list_resp=$(printf '%s' "$list_full" | tail -1)
  pool_header=$(printf '%s' "$list_full" | grep -i "^X-MethodSplit-Pool:" | tr -d '\r')
  case "$list_resp" in
  *"$id"*) ;;
  *)
    stale_with_ryw=$((stale_with_ryw + 1))
    if [ "$stale_with_ryw" -le 5 ]; then
      echo "DEBUG stale#$stale_with_ryw iter=$i id=$id pool=[$pool_header]" >&2
    fi
    ;;
  esac

  direct_resp=$(curl -s "$READ_DIRECT/v1/payments?limit=5" -H "X-Merchant-ID: $MERCHANT_ID")
  case "$direct_resp" in
  *"$id"*) ;;
  *) stale_without_ryw=$((stale_without_ryw + 1)) ;;
  esac

  if [ $((i % 25)) -eq 0 ]; then
    lag=$(curl -s "$READ_DIRECT/healthz" | grep -o '"replica_lag_seconds":[0-9.]*' | cut -d':' -f2)
    lag_int=${lag%.*}
    [ -n "$lag_int" ] && [ "$lag_int" -gt "$max_lag" ] 2>/dev/null && max_lag=$lag_int
  fi
done

echo "=== E4: read-your-writes stale-read count ==="
echo "N=$N"
echo "stale_with_ryw=$stale_with_ryw"
echo "stale_without_ryw=$stale_without_ryw"
echo "max_observed_replica_lag_seconds=$max_lag"
