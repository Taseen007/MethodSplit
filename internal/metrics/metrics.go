// Package metrics defines the Prometheus metrics exposed by the gateway at
// /metrics (ARCHITECTURE.md §13).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ms_requests_total",
		Help: "Total requests handled by the gateway.",
	}, []string{"method", "class", "pool", "status"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ms_request_duration_seconds",
		Help:    "Gateway request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"class", "pool"})

	UpstreamHealthy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ms_upstream_healthy",
		Help: "1 if the upstream target is currently healthy, else 0.",
	}, []string{"pool", "target"})

	CacheTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ms_cache_total",
		Help: "Cache lookups by result.",
	}, []string{"result"}) // hit | miss | bypass

	IdempotencyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ms_idempotency_total",
		Help: "Idempotency outcomes by result.",
	}, []string{"result"}) // new | replay | conflict | mismatch

	RateLimitRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ms_ratelimit_rejected_total",
		Help: "Requests rejected by a rate limit.",
	}, []string{"limit"}) // ip | read | write

	RedisFailOpenTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ms_redis_failopen_total",
		Help: "Times a read request proceeded without Redis because Redis was unavailable.",
	})

	MLScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "ms_ml_score",
		Help:    "Anomaly scores produced by the ML guard.",
		Buckets: prometheus.LinearBuckets(-1, 0.1, 21),
	})

	MLFlaggedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ms_ml_flagged_total",
		Help: "Requests flagged as anomalous by the ML guard, by mode.",
	}, []string{"mode"}) // shadow | enforce

	MLModelInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ms_ml_model_info",
		Help: "Always 1; the version label identifies the loaded model.",
	}, []string{"version"})
)
