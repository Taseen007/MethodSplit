package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

const (
	ModeRead  = "read"
	ModeWrite = "write"

	maxBodyBytes               = 1 << 20 // 1 MB
	defaultListLimit           = 20
	maxListLimit               = 100
	replicaLagThresholdSeconds = 10
)

type Handler struct {
	Store *Store
	Mode  string
}

func NewHandler(store *Store, mode string) *Handler {
	return &Handler{Store: store, Mode: mode}
}

// Routes builds the HTTP mux for this instance's mode. Write instances get
// the full API. Read instances only get the mux entries; net/http's method-
// aware routing (Go 1.22+) then answers any POST/PUT/DELETE on a registered
// path with 405 + Allow automatically, since no handler is registered for
// those methods.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	if h.Mode == ModeWrite {
		mux.HandleFunc("POST /v1/payments", h.CreatePayment)
		mux.HandleFunc("POST /v1/payments/{id}/execute", h.ExecutePayment)
		mux.HandleFunc("POST /v1/payments/{id}/refund", h.RefundPayment)
	}

	mux.HandleFunc("GET /v1/payments/{id}", h.GetPayment)
	mux.HandleFunc("GET /v1/payments", h.ListPayments)
	mux.HandleFunc("GET /v1/balance", h.GetBalance)
	mux.HandleFunc("GET /healthz", h.Healthz)

	return mux
}

func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.Mode == ModeWrite {
		if err := h.Store.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "mode": h.Mode, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": h.Mode})
		return
	}

	lag, err := h.Store.ReplicaLagSeconds(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "mode": h.Mode, "error": err.Error()})
		return
	}
	status, statusStr := http.StatusOK, "ok"
	if lag > replicaLagThresholdSeconds {
		status, statusStr = http.StatusServiceUnavailable, "lagging"
	}
	writeJSON(w, status, map[string]any{"status": statusStr, "mode": h.Mode, "replica_lag_seconds": lag})
}

func (h *Handler) CreatePayment(w http.ResponseWriter, r *http.Request) {
	merchantID := r.Header.Get("X-Merchant-ID")
	if merchantID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_merchant", "X-Merchant-ID header is required")
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeError(w, r, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "could not read request body")
		return
	}

	var req CreatePaymentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "malformed JSON body")
		return
	}
	if req.AmountMinor <= 0 {
		writeError(w, r, http.StatusBadRequest, "invalid_amount", "amount_minor must be > 0")
		return
	}
	if req.Currency == "" {
		req.Currency = "BDT"
	}
	if len(req.Currency) != 3 {
		writeError(w, r, http.StatusBadRequest, "invalid_currency", "currency must be a 3-letter code")
		return
	}

	hash := HashRequest(r.Method, r.URL.Path, body)
	result, err := h.Store.RunIdempotent(r.Context(), merchantID, idemKey, hash, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		p, err := h.Store.createPaymentTx(ctx, tx, merchantID, req.AmountMinor, req.Currency)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, p, nil
	})
	h.respondIdempotent(w, r, result, err)
}

func (h *Handler) ExecutePayment(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, StatusCreated, StatusCompleted)
}

func (h *Handler) RefundPayment(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, StatusCompleted, StatusRefunded)
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, from, to string) {
	merchantID := r.Header.Get("X-Merchant-ID")
	if merchantID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_merchant", "X-Merchant-ID header is required")
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeError(w, r, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	paymentID := r.PathValue("id")
	if paymentID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_id", "payment id is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "could not read request body")
		return
	}

	hash := HashRequest(r.Method, r.URL.Path, body)
	result, err := h.Store.RunIdempotent(r.Context(), merchantID, idemKey, hash, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		p, err := h.Store.transitionPaymentTx(ctx, tx, merchantID, paymentID, from, to)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, p, nil
	})
	h.respondIdempotent(w, r, result, err)
}

func (h *Handler) GetPayment(w http.ResponseWriter, r *http.Request) {
	merchantID := r.Header.Get("X-Merchant-ID")
	if merchantID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_merchant", "X-Merchant-ID header is required")
		return
	}
	id := r.PathValue("id")
	p, err := h.Store.GetPayment(r.Context(), merchantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "payment not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "unexpected server error")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) ListPayments(w http.ResponseWriter, r *http.Request) {
	merchantID := r.Header.Get("X-Merchant-ID")
	if merchantID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_merchant", "X-Merchant-ID header is required")
		return
	}
	limit := defaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	list, err := h.Store.ListPayments(r.Context(), merchantID, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "unexpected server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payments": list})
}

func (h *Handler) GetBalance(w http.ResponseWriter, r *http.Request) {
	merchantID := r.Header.Get("X-Merchant-ID")
	if merchantID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_merchant", "X-Merchant-ID header is required")
		return
	}
	b, err := h.Store.GetBalance(r.Context(), merchantID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "unexpected server error")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) respondIdempotent(w http.ResponseWriter, r *http.Request, result IdempotentResult, err error) {
	if err != nil {
		switch {
		case errors.Is(err, ErrIdempotencyKeyReused):
			writeError(w, r, http.StatusUnprocessableEntity, "idempotency_key_mismatch", "Idempotency-Key was reused with a different request")
		case errors.Is(err, ErrNotFound):
			writeError(w, r, http.StatusNotFound, "not_found", "payment not found")
		case errors.Is(err, ErrInvalidTransition):
			writeError(w, r, http.StatusConflict, "invalid_transition", "payment is not in a state that allows this action")
		default:
			writeError(w, r, http.StatusInternalServerError, "internal_error", "unexpected server error")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: r.Header.Get("X-Request-ID"),
	}})
}
