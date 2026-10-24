package gateway

import (
	"context"
	"net/http"

	"github.com/Taseen007/MethodSplit/internal/config"
	"github.com/Taseen007/MethodSplit/internal/metrics"
	"github.com/Taseen007/MethodSplit/internal/middleware"
	"github.com/Taseen007/MethodSplit/internal/redisx"
)

func (g *Gateway) handleRead(w http.ResponseWriter, rs *respState, r *http.Request, requestID string, body []byte, route Route, merchant config.Merchant, extraHeaders http.Header) {
	ctx := r.Context()

	poolName := g.choosePoolForRead(ctx, route, merchant.MerchantID)

	cacheable := route.CacheTTL > 0 && route.Consistency == config.ConsistencyEventual && poolName == PoolRead
	var cacheKey string
	if cacheable {
		version, err := g.cache.Version(ctx, merchant.MerchantID)
		if err != nil {
			metrics.RedisFailOpenTotal.Inc()
			metrics.CacheTotal.WithLabelValues("bypass").Inc()
			w.Header().Set("X-Cache", "BYPASS")
			cacheable = false
		} else {
			cacheKey = g.cache.Key(merchant.MerchantID, version, r.Method, r.URL.Path, r.URL.RawQuery)
			cachedBody, hit, err := g.cache.Get(ctx, cacheKey)
			if err != nil {
				metrics.RedisFailOpenTotal.Inc()
				metrics.CacheTotal.WithLabelValues("bypass").Inc()
				w.Header().Set("X-Cache", "BYPASS")
				cacheable = false
			} else if hit {
				metrics.CacheTotal.WithLabelValues("hit").Inc()
				rs.status, rs.pool = http.StatusOK, poolName
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				w.Header().Set("X-MethodSplit-Pool", poolName)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(cachedBody)
				return
			} else {
				metrics.CacheTotal.WithLabelValues("miss").Inc()
			}
		}
	} else {
		w.Header().Set("X-Cache", "BYPASS")
	}

	result, servedPool := g.proxyWithFallback(ctx, r, body, poolName, extraHeaders, true)
	if servedPool == "" {
		g.writeError(w, rs, requestID, http.StatusServiceUnavailable, "no_healthy_targets", "no healthy backend available")
		return
	}

	if cacheable {
		if result.StatusCode == http.StatusOK && len(result.Body) <= maxCacheableBodyBytes {
			if err := g.cache.Set(ctx, cacheKey, result.Body, route.CacheTTL); err != nil {
				g.logger.Warn("cache set failed", "error", err)
			}
		}
		w.Header().Set("X-Cache", "MISS")
	}
	g.writeUpstream(w, rs, result, servedPool, nil)
}

func (g *Gateway) handleWrite(w http.ResponseWriter, rs *respState, r *http.Request, requestID string, body []byte, route Route, merchant config.Merchant, extraHeaders http.Header) {
	ctx := r.Context()

	var idemKey string
	if route.IdempotencyRequired {
		idemKey = r.Header.Get("Idempotency-Key")
		if idemKey == "" {
			g.writeError(w, rs, requestID, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
			return
		}
		if !idempotencyKeyPattern.MatchString(idemKey) {
			g.writeError(w, rs, requestID, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be 1-64 characters of [A-Za-z0-9_-]")
			return
		}

		bodyHash := middleware.BodyHash(body)
		reserve, err := g.idempotency.Reserve(ctx, merchant.MerchantID, idemKey, bodyHash, idempotencyProcessingTTL)
		if err != nil {
			g.writeError(w, rs, requestID, http.StatusServiceUnavailable, "redis_unavailable", "cannot safely process writes without Redis")
			return
		}

		switch reserve.State {
		case redisx.IdemStateMismatch:
			metrics.IdempotencyTotal.WithLabelValues("mismatch").Inc()
			g.writeError(w, rs, requestID, http.StatusUnprocessableEntity, "idempotency_key_mismatch", "Idempotency-Key was reused with a different request")
			return
		case redisx.IdemStateProcessing:
			metrics.IdempotencyTotal.WithLabelValues("conflict").Inc()
			g.writeError(w, rs, requestID, http.StatusConflict, "idempotency_key_processing", "a request with this Idempotency-Key is still processing")
			return
		case redisx.IdemStateCompleted:
			metrics.IdempotencyTotal.WithLabelValues("replay").Inc()
			rs.status, rs.pool = reserve.Status, PoolWrite
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.Header().Set("X-MethodSplit-Pool", PoolWrite)
			w.WriteHeader(reserve.Status)
			_, _ = w.Write(reserve.Body)
			return
		default:
			metrics.IdempotencyTotal.WithLabelValues("new").Inc()
		}
	}

	result, servedPool := g.proxyWithFallback(ctx, r, body, PoolWrite, extraHeaders, false)
	if servedPool == "" {
		if route.IdempotencyRequired {
			if err := g.idempotency.Release(ctx, merchant.MerchantID, idemKey); err != nil {
				g.logger.Warn("failed to release idempotency key after failed proxy", "error", err)
			}
		}
		g.writeError(w, rs, requestID, http.StatusServiceUnavailable, "no_healthy_targets", "no healthy write backend available")
		return
	}

	if route.IdempotencyRequired {
		if result.StatusCode >= 500 {
			if err := g.idempotency.Release(ctx, merchant.MerchantID, idemKey); err != nil {
				g.logger.Warn("failed to release idempotency key after upstream error", "error", err)
			}
		} else if err := g.idempotency.Complete(ctx, merchant.MerchantID, idemKey, result.StatusCode, result.Body, idempotencyCompletedTTL); err != nil {
			g.logger.Warn("failed to store idempotency response", "error", err)
		}
	}

	if result.StatusCode >= 200 && result.StatusCode < 300 {
		if err := g.cache.BumpVersion(ctx, merchant.MerchantID); err != nil {
			g.logger.Warn("failed to bump cache version", "error", err)
		}
		if err := g.ryw.Mark(ctx, merchant.MerchantID, g.cfg.ReadYourWrites.TTL.Duration()); err != nil {
			g.logger.Warn("failed to set read-your-writes flag", "error", err)
		}
	}

	g.writeUpstream(w, rs, result, servedPool, nil)
}

// choosePoolForRead implements ARCHITECTURE.md §5.2-§5.3: strong
// consistency always goes to the write pool (which reads the primary);
// eventual consistency goes to the read pool, unless this merchant has a
// read-your-writes flag set from a recent write, in which case it also
// goes to the write pool. split_enabled: false overrides all of this and
// sends everything to the write pool (the E2 experiment baseline).
func (g *Gateway) choosePoolForRead(ctx context.Context, route Route, merchantID string) string {
	if !g.cfg.SplitEnabled {
		return PoolWrite
	}
	if route.Consistency == config.ConsistencyStrong {
		return PoolWrite
	}
	active, err := g.ryw.Active(ctx, merchantID)
	if err != nil {
		metrics.RedisFailOpenTotal.Inc()
		g.logger.Warn("redis unavailable for read-your-writes check; defaulting to read pool", "error", err)
		return PoolRead
	}
	if active {
		return PoolWrite
	}
	return PoolRead
}

// proxyWithFallback proxies to a target in poolName. For reads
// (allowFallbackToWrite=true), a GET/HEAD is retried once on a different
// healthy target if the failure was connection-level, and if the read pool
// has no healthy target at all, it falls back to the write pool
// (ARCHITECTURE.md §5.4-§5.5). Writes call this with
// allowFallbackToWrite=false and are never retried.
func (g *Gateway) proxyWithFallback(ctx context.Context, r *http.Request, body []byte, poolName string, extraHeaders http.Header, allowFallbackToWrite bool) (ProxyResult, string) {
	pool := g.pools[poolName]
	target := pool.Next()
	if target == nil {
		if allowFallbackToWrite && poolName == PoolRead {
			return g.proxyWithFallback(ctx, r, body, PoolWrite, extraHeaders, false)
		}
		return ProxyResult{}, ""
	}

	result := g.proxier.Do(ctx, r, body, target, extraHeaders)
	if result.ConnError {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if target2 := pool.Next(); target2 != nil && target2 != target {
				if retryResult := g.proxier.Do(ctx, r, body, target2, extraHeaders); !retryResult.ConnError {
					return retryResult, poolName
				}
			}
		}
		if allowFallbackToWrite && poolName == PoolRead {
			return g.proxyWithFallback(ctx, r, body, PoolWrite, extraHeaders, false)
		}
		return ProxyResult{}, ""
	}
	return result, poolName
}
