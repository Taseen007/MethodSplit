package config

import (
	"path/filepath"
	"testing"
)

func TestLoadGateway_RealConfig(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "gateway.yaml")
	cfg, err := LoadGateway(path)
	if err != nil {
		t.Fatalf("LoadGateway(%s) failed: %v", path, err)
	}
	if !cfg.SplitEnabled {
		t.Errorf("expected split_enabled true in the shipped config")
	}
	if len(cfg.Routes) == 0 {
		t.Errorf("expected at least one route")
	}
}

func TestGateway_Validate(t *testing.T) {
	base := func() Gateway {
		return Gateway{
			Pools: Pools{
				Write: Pool{Targets: []string{"http://write:9000"}},
				Read:  Pool{Targets: []string{"http://read:9000"}},
			},
			SplitEnabled: true,
			Routes: []Route{
				{Method: "POST", Path: "/v1/payments", Class: ClassWrite, Idempotency: IdempotencyRequired},
				{Method: "GET", Path: "/v1/payments/{id}", Class: ClassRead, Consistency: ConsistencyStrong},
			},
			RateLimits: RateLimits{PerIPPerMin: 1, PerMerchantReadPerMin: 1, PerMerchantWritePerMin: 1},
			ReadYourWrites: ReadYourWrites{
				TTL: Duration(5),
			},
			Timeouts: Timeouts{Upstream: Duration(5)},
			ML:       ML{Mode: MLModeOff},
		}
	}

	t.Run("valid", func(t *testing.T) {
		cfg := base()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected valid config, got: %v", err)
		}
	})

	t.Run("empty write pool", func(t *testing.T) {
		cfg := base()
		cfg.Pools.Write.Targets = nil
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for empty write pool")
		}
	})

	t.Run("split enabled but no read pool", func(t *testing.T) {
		cfg := base()
		cfg.Pools.Read.Targets = nil
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for missing read pool when split enabled")
		}
	})

	t.Run("bad method", func(t *testing.T) {
		cfg := base()
		cfg.Routes[0].Method = "FETCH"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for invalid method")
		}
	})

	t.Run("path without leading slash", func(t *testing.T) {
		cfg := base()
		cfg.Routes[0].Path = "v1/payments"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for path without leading slash")
		}
	})

	t.Run("duplicate route", func(t *testing.T) {
		cfg := base()
		cfg.Routes = append(cfg.Routes, cfg.Routes[0])
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for duplicate route")
		}
	})

	t.Run("read route missing consistency", func(t *testing.T) {
		cfg := base()
		cfg.Routes[1].Consistency = ""
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for read route without consistency")
		}
	})

	t.Run("idempotency required on read route", func(t *testing.T) {
		cfg := base()
		cfg.Routes[1].Idempotency = IdempotencyRequired
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for idempotency required on a read route")
		}
	})

	t.Run("invalid ml mode", func(t *testing.T) {
		cfg := base()
		cfg.ML.Mode = "sometimes"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for invalid ml.mode")
		}
	})

	t.Run("non-positive rate limit", func(t *testing.T) {
		cfg := base()
		cfg.RateLimits.PerIPPerMin = 0
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for zero rate limit")
		}
	})
}

func TestDuration_UnmarshalYAML(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "gateway.yaml")
	cfg, err := LoadGateway(path)
	if err != nil {
		t.Fatalf("LoadGateway failed: %v", err)
	}
	if cfg.Timeouts.Upstream.Duration().Seconds() != 5 {
		t.Errorf("expected upstream timeout of 5s, got %v", cfg.Timeouts.Upstream.Duration())
	}
	if cfg.ReadYourWrites.TTL.Duration().Seconds() != 5 {
		t.Errorf("expected read_your_writes ttl of 5s, got %v", cfg.ReadYourWrites.TTL.Duration())
	}
}
