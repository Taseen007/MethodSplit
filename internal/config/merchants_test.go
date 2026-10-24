package config

import (
	"path/filepath"
	"testing"
)

func TestLoadMerchants_RealConfig(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "merchants.yaml")
	m, err := LoadMerchants(path)
	if err != nil {
		t.Fatalf("LoadMerchants(%s) failed: %v", path, err)
	}
	if len(m.Merchants) < 2 {
		t.Fatalf("expected at least 2 demo merchants, got %d", len(m.Merchants))
	}

	idx := m.ByAPIKey()
	demo, ok := idx["mk_demo_a"]
	if !ok {
		t.Fatalf("expected mk_demo_a to be present")
	}
	if demo.MerchantID != "merchant_demo_a" {
		t.Errorf("expected merchant_demo_a, got %s", demo.MerchantID)
	}
}

func TestMerchants_Validate(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		m := Merchants{}
		if err := m.Validate(); err == nil {
			t.Fatalf("expected error for empty merchants")
		}
	})

	t.Run("duplicate api key", func(t *testing.T) {
		m := Merchants{Merchants: []Merchant{
			{MerchantID: "a", APIKey: "k1", Secret: "s1"},
			{MerchantID: "b", APIKey: "k1", Secret: "s2"},
		}}
		if err := m.Validate(); err == nil {
			t.Fatalf("expected error for duplicate api_key")
		}
	})

	t.Run("duplicate merchant id", func(t *testing.T) {
		m := Merchants{Merchants: []Merchant{
			{MerchantID: "a", APIKey: "k1", Secret: "s1"},
			{MerchantID: "a", APIKey: "k2", Secret: "s2"},
		}}
		if err := m.Validate(); err == nil {
			t.Fatalf("expected error for duplicate merchant_id")
		}
	})

	t.Run("missing secret", func(t *testing.T) {
		m := Merchants{Merchants: []Merchant{{MerchantID: "a", APIKey: "k1"}}}
		if err := m.Validate(); err == nil {
			t.Fatalf("expected error for missing secret")
		}
	})

	t.Run("valid", func(t *testing.T) {
		m := Merchants{Merchants: []Merchant{{MerchantID: "a", APIKey: "k1", Secret: "s1"}}}
		if err := m.Validate(); err != nil {
			t.Fatalf("expected valid, got: %v", err)
		}
	})
}
