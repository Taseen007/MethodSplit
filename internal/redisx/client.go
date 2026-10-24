// Package redisx wraps the gateway's Redis usage: sliding-window rate
// limits, the idempotency fast path, response caching, and read-your-writes
// tracking. Every operation here is on the gateway's hot path, so timeouts
// are kept short — callers are expected to apply the fail-open/fail-closed
// policy from ARCHITECTURE.md §10 when a call errors.
package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type Client struct {
	rdb *redis.Client
}

// New builds a Redis client tuned for the gateway's fail-open/fail-closed
// policy (ARCHITECTURE.md §10) to actually be fast: the default go-redis
// timeouts (seconds, with automatic retries) mean a Redis outage would
// otherwise make every request hang for many seconds — or, once a request
// chains several Redis calls (rate limit, RYW, cache), tens of seconds —
// before "failing open" at all. Short timeouts and no retries make an
// outage surface as an immediate error instead.
func New(addr string) *Client {
	return &Client{rdb: redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		// PoolTimeout defaults to ReadTimeout+1s, which would dominate the
		// short timeouts above; set it explicitly so it doesn't.
		PoolTimeout: 200 * time.Millisecond,
		MaxRetries:  -1, // disabled at the command level; the caller's fail-open/fail-closed policy handles it
		// The connection pool itself also retries a failed dial (default: 5
		// attempts with backoff) independently of MaxRetries above. Left at
		// the default, one failed command could spend several seconds just
		// re-dialing before MaxRetries=-1 even comes into play — one attempt
		// is enough here, since the caller is about to fail open/closed anyway.
		DialerRetries: 1,
	})}
}

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Client) Close() error {
	return c.rdb.Close()
}

// Raw exposes the underlying go-redis client for callers (internal/ml) that
// need primitives this package doesn't wrap, like pipelining and HyperLogLog.
func (c *Client) Raw() *redis.Client {
	return c.rdb
}
