package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	IdemStateNew        = "new"
	IdemStateProcessing = "processing"
	IdemStateCompleted  = "completed"
	IdemStateMismatch   = "mismatch"
)

// reserveScript is the gateway-side idempotency fast path (ARCHITECTURE.md
// §8): the first request for a key claims it (state=PROCESSING) atomically,
// so concurrent duplicates see PROCESSING instead of racing to both proceed.
var reserveScript = redis.NewScript(`
local key = KEYS[1]
local hash = ARGV[1]
local processing_ttl = tonumber(ARGV[2])

local state = redis.call('HGET', key, 'state')
if state == false then
	redis.call('HSET', key, 'state', 'PROCESSING', 'hash', hash)
	redis.call('EXPIRE', key, processing_ttl)
	return {'new'}
end

local stored_hash = redis.call('HGET', key, 'hash')
if stored_hash ~= hash then
	return {'mismatch'}
end

if state == 'PROCESSING' then
	return {'processing'}
end

local status = redis.call('HGET', key, 'status')
local body = redis.call('HGET', key, 'body')
return {'completed', status, body}
`)

type ReserveResult struct {
	State  string
	Status int
	Body   []byte
}

type Idempotency struct {
	client *Client
}

func NewIdempotency(client *Client) *Idempotency {
	return &Idempotency{client: client}
}

func idemKey(merchantID, key string) string {
	return fmt.Sprintf("idem:%s:%s", merchantID, key)
}

// Reserve claims (merchantID, key) for this request, or reports why it
// can't: another request with the same key is still PROCESSING, the key was
// reused with a different body hash, or it already COMPLETED (with the
// response to replay).
func (i *Idempotency) Reserve(ctx context.Context, merchantID, key, bodyHash string, processingTTL time.Duration) (ReserveResult, error) {
	res, err := reserveScript.Run(ctx, i.client.rdb, []string{idemKey(merchantID, key)}, bodyHash, int(processingTTL.Seconds())).Slice()
	if err != nil {
		return ReserveResult{}, err
	}
	state, _ := res[0].(string)
	out := ReserveResult{State: state}
	if state == IdemStateCompleted {
		statusStr, _ := res[1].(string)
		fmt.Sscanf(statusStr, "%d", &out.Status)
		if body, ok := res[2].(string); ok {
			out.Body = []byte(body)
		}
	}
	return out, nil
}

// Complete stores the final response so a later replay can return it, and
// lets the PROCESSING lock expire into a long-lived COMPLETED record.
func (i *Idempotency) Complete(ctx context.Context, merchantID, key string, status int, body []byte, completedTTL time.Duration) error {
	k := idemKey(merchantID, key)
	if err := i.client.rdb.HSet(ctx, k, "state", IdemStateCompleted, "status", status, "body", body).Err(); err != nil {
		return err
	}
	return i.client.rdb.Expire(ctx, k, completedTTL).Err()
}

// Release deletes the PROCESSING record after an upstream failure (5xx or
// timeout), so the client can retry with the same Idempotency-Key instead of
// being stuck behind a lock for a request that never completed.
func (i *Idempotency) Release(ctx context.Context, merchantID, key string) error {
	return i.client.rdb.Del(ctx, idemKey(merchantID, key)).Err()
}
