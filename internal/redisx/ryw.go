package redisx

import (
	"context"
	"time"
)

// RYW implements read-your-writes (ARCHITECTURE.md §5.3): right after a
// merchant's write, their eventual reads are routed to the write pool
// (which reads the primary) for a short TTL, so they never see a stale
// reply from a replica that hasn't caught up yet.
type RYW struct {
	client *Client
}

func NewRYW(client *Client) *RYW {
	return &RYW{client: client}
}

func rywKey(merchantID string) string {
	return "ryw:" + merchantID
}

func (r *RYW) Mark(ctx context.Context, merchantID string, ttl time.Duration) error {
	return r.client.rdb.Set(ctx, rywKey(merchantID), "1", ttl).Err()
}

func (r *RYW) Active(ctx context.Context, merchantID string) (bool, error) {
	n, err := r.client.rdb.Exists(ctx, rywKey(merchantID)).Result()
	return n > 0, err
}
