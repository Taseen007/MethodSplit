package gateway

import "context"

// RecordInput bundles what the ML guard needs to log and update feature
// windows for one completed request.
type RecordInput struct {
	APIKey     string
	Route      string
	RequestID  string
	IsWrite    bool
	StatusCode int
	PaymentID  string
	Score      float64
	Flagged    bool
}

// MLGuard scores requests for anomalous clients (ARCHITECTURE.md §12) and
// tracks the per-key feature windows the scorer reads from. NoOpMLGuard
// implements this as an always-off no-op; internal/ml provides the real
// Isolation Forest-backed implementation used when ml.mode isn't "off".
type MLGuard interface {
	Mode() string
	// Score returns the current anomaly score and flag for apiKey. Only
	// meaningful (and only called by the gateway) when Mode() != "off".
	Score(ctx context.Context, apiKey string) (score float64, flagged bool, err error)
	// RecordAuthFailure updates the auth-failure feature for apiKey.
	RecordAuthFailure(ctx context.Context, apiKey string)
	// Record updates the feature windows and appends to the feature log.
	// Called for every authenticated request, regardless of mode.
	Record(ctx context.Context, in RecordInput)
	// IsTightened reports whether merchantID is currently under the
	// enforce-mode rate-limit cut.
	IsTightened(ctx context.Context, merchantID string) (bool, error)
	// Tighten applies the enforce-mode rate-limit cut to merchantID.
	Tighten(ctx context.Context, merchantID string) error
}

type NoOpMLGuard struct{}

func (NoOpMLGuard) Mode() string { return "off" }
func (NoOpMLGuard) Score(ctx context.Context, apiKey string) (float64, bool, error) {
	return 0, false, nil
}
func (NoOpMLGuard) RecordAuthFailure(ctx context.Context, apiKey string) {}
func (NoOpMLGuard) Record(ctx context.Context, in RecordInput)           {}
func (NoOpMLGuard) IsTightened(ctx context.Context, merchantID string) (bool, error) {
	return false, nil
}
func (NoOpMLGuard) Tighten(ctx context.Context, merchantID string) error { return nil }
