package payments

import "errors"

var (
	// ErrNotFound is returned when a payment doesn't exist for the given merchant.
	ErrNotFound = errors.New("payment not found")

	// ErrInvalidTransition is returned when execute/refund is attempted from a
	// status that doesn't allow it (e.g. refunding a payment that was never completed).
	ErrInvalidTransition = errors.New("invalid payment state transition")

	// ErrIdempotencyKeyReused is returned when the same idempotency key is
	// reused with a different request body (method + path + body hash).
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different request")
)
