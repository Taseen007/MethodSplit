// Package config loads and validates the gateway's route table
// (gateway.yaml) and merchant credentials (merchants.yaml).
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Gateway struct {
	SplitEnabled   bool           `yaml:"split_enabled"`
	Pools          Pools          `yaml:"pools"`
	Routes         []Route        `yaml:"routes"`
	RateLimits     RateLimits     `yaml:"rate_limits"`
	ReadYourWrites ReadYourWrites `yaml:"read_your_writes"`
	Timeouts       Timeouts       `yaml:"timeouts"`
	HealthCheck    HealthCheck    `yaml:"health_check"`
	ML             ML             `yaml:"ml"`
}

type Pools struct {
	Read  Pool `yaml:"read"`
	Write Pool `yaml:"write"`
}

type Pool struct {
	Targets []string `yaml:"targets"`
}

type Route struct {
	Method      string   `yaml:"method"`
	Path        string   `yaml:"path"`
	Class       string   `yaml:"class"`
	Idempotency string   `yaml:"idempotency,omitempty"`
	Consistency string   `yaml:"consistency,omitempty"`
	CacheTTL    Duration `yaml:"cache_ttl,omitempty"`
}

const (
	ClassRead  = "read"
	ClassWrite = "write"

	ConsistencyStrong   = "strong"
	ConsistencyEventual = "eventual"

	IdempotencyRequired = "required"
)

type RateLimits struct {
	PerIPPerMin            int `yaml:"per_ip_per_min"`
	PerMerchantReadPerMin  int `yaml:"per_merchant_read_per_min"`
	PerMerchantWritePerMin int `yaml:"per_merchant_write_per_min"`
}

type ReadYourWrites struct {
	TTL Duration `yaml:"ttl"`
}

type Timeouts struct {
	Upstream Duration `yaml:"upstream"`
}

type HealthCheck struct {
	Path           string   `yaml:"path"`
	Interval       Duration `yaml:"interval"`
	Timeout        Duration `yaml:"timeout"`
	UnhealthyAfter int      `yaml:"unhealthy_after"`
	HealthyAfter   int      `yaml:"healthy_after"`
}

const (
	MLModeOff     = "off"
	MLModeShadow  = "shadow"
	MLModeEnforce = "enforce"
)

type ML struct {
	Mode      string `yaml:"mode"`
	ModelPath string `yaml:"model_path"`
}

// LoadGateway reads and validates the gateway route-table config at path.
// It fails fast: any structural or semantic problem is returned as an error
// instead of letting the gateway start with a config it can't safely serve.
func LoadGateway(path string) (*Gateway, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gateway config: %w", err)
	}

	var cfg Gateway
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse gateway config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid gateway config: %w", err)
	}

	return &cfg, nil
}

var validMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true,
	"PATCH": true, "DELETE": true, "OPTIONS": true,
}

func (c *Gateway) Validate() error {
	if len(c.Pools.Write.Targets) == 0 {
		return fmt.Errorf("pools.write.targets must not be empty")
	}
	if c.SplitEnabled && len(c.Pools.Read.Targets) == 0 {
		return fmt.Errorf("pools.read.targets must not be empty when split_enabled is true")
	}

	if len(c.Routes) == 0 {
		return fmt.Errorf("routes must not be empty")
	}

	seen := make(map[string]bool, len(c.Routes))
	for i, r := range c.Routes {
		if !validMethods[strings.ToUpper(r.Method)] {
			return fmt.Errorf("route[%d]: invalid method %q", i, r.Method)
		}
		if !strings.HasPrefix(r.Path, "/") {
			return fmt.Errorf("route[%d]: path %q must start with /", i, r.Path)
		}
		key := strings.ToUpper(r.Method) + " " + r.Path
		if seen[key] {
			return fmt.Errorf("route[%d]: duplicate route %s", i, key)
		}
		seen[key] = true

		switch r.Class {
		case ClassRead:
			if r.Consistency != ConsistencyStrong && r.Consistency != ConsistencyEventual {
				return fmt.Errorf("route[%d]: read route needs consistency strong|eventual, got %q", i, r.Consistency)
			}
		case ClassWrite:
			// no additional constraint; idempotency is optional but expected on payment writes
		default:
			return fmt.Errorf("route[%d]: class must be %q or %q, got %q", i, ClassRead, ClassWrite, r.Class)
		}

		if r.Idempotency != "" && r.Idempotency != IdempotencyRequired {
			return fmt.Errorf("route[%d]: idempotency must be empty or %q, got %q", i, IdempotencyRequired, r.Idempotency)
		}
		if r.Idempotency == IdempotencyRequired && r.Class != ClassWrite {
			return fmt.Errorf("route[%d]: idempotency required only makes sense on write routes", i)
		}
	}

	if c.RateLimits.PerIPPerMin <= 0 || c.RateLimits.PerMerchantReadPerMin <= 0 || c.RateLimits.PerMerchantWritePerMin <= 0 {
		return fmt.Errorf("rate_limits: all limits must be positive")
	}

	if c.ReadYourWrites.TTL.Duration() <= 0 {
		return fmt.Errorf("read_your_writes.ttl must be positive")
	}

	if c.Timeouts.Upstream.Duration() <= 0 {
		return fmt.Errorf("timeouts.upstream must be positive")
	}

	switch c.ML.Mode {
	case MLModeOff, MLModeShadow, MLModeEnforce:
	default:
		return fmt.Errorf("ml.mode must be one of off|shadow|enforce, got %q", c.ML.Mode)
	}

	return nil
}
