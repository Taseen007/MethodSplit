// Package payments implements the mock payment backend (MODE=read|write)
// that sits behind the MethodSplit gateway.
package payments

import "time"

const (
	StatusCreated   = "CREATED"
	StatusCompleted = "COMPLETED"
	StatusRefunded  = "REFUNDED"
	StatusFailed    = "FAILED"
)

type Payment struct {
	ID          string    `json:"id"`
	MerchantID  string    `json:"merchant_id"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Balance struct {
	MerchantID  string `json:"merchant_id"`
	AmountMinor int64  `json:"amount_minor"`
}

type CreatePaymentRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}
