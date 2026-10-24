package gateway

import (
	"fmt"
	"net/url"
	"sync/atomic"
)

const (
	PoolRead  = "read"
	PoolWrite = "write"
)

// Target is one backend instance in a pool. Health state is tracked with
// hysteresis (N consecutive failures to go down, N consecutive successes to
// come back) so a single flaky check doesn't flap it.
type Target struct {
	URL *url.URL

	healthy          atomic.Bool
	consecutiveFails atomic.Int32
	consecutiveOK    atomic.Int32
}

func (t *Target) Healthy() bool {
	return t.healthy.Load()
}

// recordCheck applies one health-check result. unhealthyAfter/healthyAfter
// are the consecutive-result thresholds from configs/gateway.yaml.
func (t *Target) recordCheck(ok bool, unhealthyAfter, healthyAfter int) {
	if ok {
		t.consecutiveFails.Store(0)
		oks := t.consecutiveOK.Add(1)
		if !t.healthy.Load() && int(oks) >= healthyAfter {
			t.healthy.Store(true)
		}
		return
	}
	t.consecutiveOK.Store(0)
	fails := t.consecutiveFails.Add(1)
	if t.healthy.Load() && int(fails) >= unhealthyAfter {
		t.healthy.Store(false)
	}
}

// Pool is a set of backend targets, load-balanced round-robin over whichever
// are currently healthy.
type Pool struct {
	Name    string
	targets []*Target
	counter atomic.Uint64
}

func NewPool(name string, rawTargets []string) (*Pool, error) {
	targets := make([]*Target, 0, len(rawTargets))
	for _, raw := range rawTargets {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("pool %s: invalid target %q: %w", name, raw, err)
		}
		t := &Target{URL: u}
		t.healthy.Store(true) // assumed healthy until the first check says otherwise
		targets = append(targets, t)
	}
	return &Pool{Name: name, targets: targets}, nil
}

func (p *Pool) Targets() []*Target {
	return p.targets
}

// Next returns the next healthy target via round robin, or nil if every
// target in the pool is currently unhealthy.
func (p *Pool) Next() *Target {
	n := len(p.targets)
	if n == 0 {
		return nil
	}
	start := p.counter.Add(1)
	for i := 0; i < n; i++ {
		idx := (start + uint64(i)) % uint64(n)
		t := p.targets[idx]
		if t.Healthy() {
			return t
		}
	}
	return nil
}
