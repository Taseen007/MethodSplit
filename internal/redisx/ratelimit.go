package redisx

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript implements a sliding-window counter as one atomic Lua
// script (ARCHITECTURE.md §7): trim entries older than the window, count
// what's left, and only add the new entry if that count is under the limit.
// Doing the check-and-increment in one script is what makes it race-safe
// under concurrent requests.
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window_ms)
local count = redis.call('ZCARD', key)

if count >= limit then
	local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
	local retry_after_ms = 0
	if oldest[2] ~= nil then
		retry_after_ms = (tonumber(oldest[2]) + window_ms) - now
		if retry_after_ms < 0 then
			retry_after_ms = 0
		end
	end
	return {0, count, retry_after_ms}
end

redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, window_ms)
return {1, count + 1, 0}
`)

type RateLimitResult struct {
	Allowed    bool
	Count      int64
	Limit      int64
	RetryAfter time.Duration
}

type RateLimiter struct {
	client  *Client
	counter atomic.Uint64
}

func NewRateLimiter(client *Client) *RateLimiter {
	return &RateLimiter{client: client}
}

// Allow checks and, if allowed, records one request against key within the
// given window/limit. It's safe for concurrent use.
func (r *RateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error) {
	now := time.Now().UnixMilli()
	member := fmt.Sprintf("%d-%d", now, r.counter.Add(1))

	res, err := slidingWindowScript.Run(ctx, r.client.rdb, []string{key}, now, window.Milliseconds(), limit, member).Slice()
	if err != nil {
		return RateLimitResult{}, err
	}
	allowed, _ := res[0].(int64)
	count, _ := res[1].(int64)
	retryAfterMs, _ := res[2].(int64)

	return RateLimitResult{
		Allowed:    allowed == 1,
		Count:      count,
		Limit:      int64(limit),
		RetryAfter: time.Duration(retryAfterMs) * time.Millisecond,
	}, nil
}
