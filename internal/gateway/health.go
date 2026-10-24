package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/Taseen007/MethodSplit/internal/config"
	"github.com/Taseen007/MethodSplit/internal/metrics"
)

// HealthChecker actively polls every target in every pool at a fixed
// interval (ARCHITECTURE.md §5.4): GET <target>/healthz, 200 = healthy.
type HealthChecker struct {
	client         *http.Client
	path           string
	interval       time.Duration
	unhealthyAfter int
	healthyAfter   int
}

func NewHealthChecker(cfg config.HealthCheck) *HealthChecker {
	return &HealthChecker{
		client:         &http.Client{Timeout: cfg.Timeout.Duration()},
		path:           cfg.Path,
		interval:       cfg.Interval.Duration(),
		unhealthyAfter: cfg.UnhealthyAfter,
		healthyAfter:   cfg.HealthyAfter,
	}
}

// Run polls every pool's targets until ctx is cancelled. It checks
// immediately on start, then on every tick.
func (hc *HealthChecker) Run(ctx context.Context, pools map[string]*Pool) {
	hc.checkAll(ctx, pools)

	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkAll(ctx, pools)
		}
	}
}

func (hc *HealthChecker) checkAll(ctx context.Context, pools map[string]*Pool) {
	for poolName, pool := range pools {
		for _, t := range pool.Targets() {
			go hc.check(ctx, poolName, t)
		}
	}
}

func (hc *HealthChecker) check(ctx context.Context, poolName string, t *Target) {
	ok := hc.probe(ctx, t)
	t.recordCheck(ok, hc.unhealthyAfter, hc.healthyAfter)
	metrics.UpstreamHealthy.WithLabelValues(poolName, t.URL.String()).Set(boolToFloat(t.Healthy()))
}

func (hc *HealthChecker) probe(ctx context.Context, t *Target) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL.String()+hc.path, nil)
	if err != nil {
		return false
	}
	resp, err := hc.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
