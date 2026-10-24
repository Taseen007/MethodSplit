package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Merchant struct {
	MerchantID string `yaml:"merchant_id"`
	APIKey     string `yaml:"api_key"`
	Secret     string `yaml:"secret"`
}

type Merchants struct {
	Merchants []Merchant `yaml:"merchants"`
}

// LoadMerchants reads and validates the demo merchant credentials at path.
func LoadMerchants(path string) (*Merchants, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read merchants config: %w", err)
	}

	var m Merchants
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse merchants config: %w", err)
	}

	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid merchants config: %w", err)
	}

	return &m, nil
}

func (m *Merchants) Validate() error {
	if len(m.Merchants) == 0 {
		return fmt.Errorf("merchants must not be empty")
	}
	seenKeys := make(map[string]bool, len(m.Merchants))
	seenIDs := make(map[string]bool, len(m.Merchants))
	for i, merchant := range m.Merchants {
		if merchant.MerchantID == "" {
			return fmt.Errorf("merchants[%d]: merchant_id must not be empty", i)
		}
		if merchant.APIKey == "" {
			return fmt.Errorf("merchants[%d]: api_key must not be empty", i)
		}
		if merchant.Secret == "" {
			return fmt.Errorf("merchants[%d]: secret must not be empty", i)
		}
		if seenKeys[merchant.APIKey] {
			return fmt.Errorf("merchants[%d]: duplicate api_key %q", i, merchant.APIKey)
		}
		seenKeys[merchant.APIKey] = true
		if seenIDs[merchant.MerchantID] {
			return fmt.Errorf("merchants[%d]: duplicate merchant_id %q", i, merchant.MerchantID)
		}
		seenIDs[merchant.MerchantID] = true
	}
	return nil
}

// ByAPIKey indexes merchants by their API key for O(1) auth lookups.
func (m *Merchants) ByAPIKey() map[string]Merchant {
	idx := make(map[string]Merchant, len(m.Merchants))
	for _, merchant := range m.Merchants {
		idx[merchant.APIKey] = merchant
	}
	return idx
}
