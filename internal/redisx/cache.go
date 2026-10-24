package redisx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache implements the merchant-scoped, version-invalidated response cache
// from ARCHITECTURE.md §9. Bumping cachever:{merchant} after a write makes
// every previously cached key for that merchant stop matching instantly,
// with no key scanning needed; old entries simply expire by TTL.
type Cache struct {
	client *Client
}

func NewCache(client *Client) *Cache {
	return &Cache{client: client}
}

func (c *Cache) Version(ctx context.Context, merchantID string) (int64, error) {
	v, err := c.client.rdb.Get(ctx, "cachever:"+merchantID).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return v, err
}

func (c *Cache) BumpVersion(ctx context.Context, merchantID string) error {
	return c.client.rdb.Incr(ctx, "cachever:"+merchantID).Err()
}

// Key builds the cache key for a request. The merchant ID is always part of
// it — without that, one merchant could be served another merchant's
// cached data.
func (c *Cache) Key(merchantID string, version int64, method, path, query string) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte("\n"))
	h.Write([]byte(path))
	h.Write([]byte("\n"))
	h.Write([]byte(query))
	return fmt.Sprintf("cache:%s:v%d:%s", merchantID, version, hex.EncodeToString(h.Sum(nil)))
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	body, err := c.client.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

func (c *Cache) Set(ctx context.Context, key string, body []byte, ttl time.Duration) error {
	return c.client.rdb.Set(ctx, key, body, ttl).Err()
}
