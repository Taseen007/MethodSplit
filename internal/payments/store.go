package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isInvalidID reports whether err is Postgres complaining that paymentID
// isn't a syntactically valid UUID (SQLSTATE 22P02). A client can send any
// string in a path segment — e.g. an enumeration attempt trying random
// non-UUID strings — and that should look like "not found" (404), not an
// internal error (500).
func isInvalidID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// ReplicaLagSeconds reports streaming-replication lag. It is 0 whenever the
// replica has applied everything it has received (pg_last_wal_receive_lsn()
// == pg_last_wal_replay_lsn()) — including while idle with no recent writes,
// since "no new WAL to apply" is not the same as "falling behind". Only when
// there is real unapplied backlog does it fall back to
// now() - pg_last_xact_replay_timestamp() as the lag estimate. Using that
// timestamp alone (without the backlog check) would report ever-growing
// "lag" during any idle period, which is a false positive, not real lag.
func (s *Store) ReplicaLagSeconds(ctx context.Context) (float64, error) {
	var lag *float64
	err := s.pool.QueryRow(ctx, `
		SELECT CASE
			WHEN pg_last_wal_receive_lsn() IS NOT DISTINCT FROM pg_last_wal_replay_lsn() THEN 0
			WHEN pg_last_xact_replay_timestamp() IS NULL THEN 0
			ELSE EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))
		END
	`).Scan(&lag)
	if err != nil {
		return 0, err
	}
	if lag == nil {
		return 0, nil
	}
	return *lag, nil
}

// HashRequest is the request fingerprint used to detect an idempotency key
// being reused for a different request (ARCHITECTURE.md §8).
func HashRequest(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte("\n"))
	h.Write([]byte(path))
	h.Write([]byte("\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

type IdempotentResult struct {
	Status   int
	Body     json.RawMessage
	Replayed bool
}

// RunIdempotent runs fn exactly once per (merchantID, key, requestHash),
// inside one database transaction. A concurrent call with the same key
// blocks on the row lock until the first one commits or rolls back, then
// replays its stored response instead of running fn again. A call that
// reuses the key with a different requestHash gets ErrIdempotencyKeyReused.
func (s *Store) RunIdempotent(
	ctx context.Context,
	merchantID, key, requestHash string,
	fn func(ctx context.Context, tx pgx.Tx) (status int, body any, err error),
) (IdempotentResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IdempotentResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_records (merchant_id, key, request_hash, response_status, response_body)
		VALUES ($1, $2, $3, 0, '{}'::jsonb)
		ON CONFLICT (merchant_id, key) DO NOTHING
	`, merchantID, key, requestHash)
	if err != nil {
		return IdempotentResult{}, fmt.Errorf("reserve idempotency key: %w", err)
	}

	if tag.RowsAffected() == 1 {
		status, body, ferr := fn(ctx, tx)
		if ferr != nil {
			return IdempotentResult{}, ferr
		}
		bodyJSON, merr := json.Marshal(body)
		if merr != nil {
			return IdempotentResult{}, fmt.Errorf("marshal response body: %w", merr)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE idempotency_records SET response_status = $3, response_body = $4
			WHERE merchant_id = $1 AND key = $2
		`, merchantID, key, status, bodyJSON); err != nil {
			return IdempotentResult{}, fmt.Errorf("store idempotency response: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return IdempotentResult{}, fmt.Errorf("commit tx: %w", err)
		}
		return IdempotentResult{Status: status, Body: bodyJSON, Replayed: false}, nil
	}

	// Someone else holds this key. Our INSERT blocked on their row lock until
	// they committed or rolled back, so this read now sees the final state.
	var storedHash string
	var storedStatus int
	var storedBody []byte
	err = tx.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body
		FROM idempotency_records WHERE merchant_id = $1 AND key = $2
	`, merchantID, key).Scan(&storedHash, &storedStatus, &storedBody)
	if err != nil {
		return IdempotentResult{}, fmt.Errorf("read existing idempotency record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IdempotentResult{}, fmt.Errorf("commit tx: %w", err)
	}
	if storedHash != requestHash {
		return IdempotentResult{}, ErrIdempotencyKeyReused
	}
	return IdempotentResult{Status: storedStatus, Body: storedBody, Replayed: true}, nil
}

func (s *Store) createPaymentTx(ctx context.Context, tx pgx.Tx, merchantID string, amountMinor int64, currency string) (Payment, error) {
	var p Payment
	err := tx.QueryRow(ctx, `
		INSERT INTO payments (id, merchant_id, amount_minor, currency, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, merchant_id, amount_minor, currency, status, created_at, updated_at
	`, uuid.NewString(), merchantID, amountMinor, currency, StatusCreated).Scan(
		&p.ID, &p.MerchantID, &p.AmountMinor, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt,
	)
	return p, err
}

func (s *Store) transitionPaymentTx(ctx context.Context, tx pgx.Tx, merchantID, paymentID, fromStatus, toStatus string) (Payment, error) {
	var p Payment
	err := tx.QueryRow(ctx, `
		UPDATE payments SET status = $1, updated_at = now()
		WHERE id = $2 AND merchant_id = $3 AND status = $4
		RETURNING id, merchant_id, amount_minor, currency, status, created_at, updated_at
	`, toStatus, paymentID, merchantID, fromStatus).Scan(
		&p.ID, &p.MerchantID, &p.AmountMinor, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt,
	)
	if isInvalidID(err) {
		return Payment{}, ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		exists, existsErr := s.paymentExistsTx(ctx, tx, merchantID, paymentID)
		if existsErr != nil {
			return Payment{}, existsErr
		}
		if !exists {
			return Payment{}, ErrNotFound
		}
		return Payment{}, ErrInvalidTransition
	}
	return p, err
}

func (s *Store) paymentExistsTx(ctx context.Context, tx pgx.Tx, merchantID, paymentID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payments WHERE id = $1 AND merchant_id = $2)`, paymentID, merchantID).Scan(&exists)
	if isInvalidID(err) {
		return false, nil
	}
	return exists, err
}

func (s *Store) GetPayment(ctx context.Context, merchantID, paymentID string) (Payment, error) {
	var p Payment
	err := s.pool.QueryRow(ctx, `
		SELECT id, merchant_id, amount_minor, currency, status, created_at, updated_at
		FROM payments WHERE id = $1 AND merchant_id = $2
	`, paymentID, merchantID).Scan(&p.ID, &p.MerchantID, &p.AmountMinor, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidID(err) {
		return Payment{}, ErrNotFound
	}
	return p, err
}

func (s *Store) ListPayments(ctx context.Context, merchantID string, limit int) ([]Payment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, merchant_id, amount_minor, currency, status, created_at, updated_at
		FROM payments WHERE merchant_id = $1
		ORDER BY created_at DESC LIMIT $2
	`, merchantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Payment, 0)
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.MerchantID, &p.AmountMinor, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetBalance(ctx context.Context, merchantID string) (Balance, error) {
	// The payments table holds each payment's current status, not a ledger of
	// state transitions. A refunded payment's status is REFUNDED, not
	// COMPLETED, so it already falls out of this sum on its own — there is no
	// separate REFUNDED term to subtract (that would double-count it).
	var amount int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor) FILTER (WHERE status = 'COMPLETED'), 0)
		FROM payments WHERE merchant_id = $1
	`, merchantID).Scan(&amount)
	return Balance{MerchantID: merchantID, AmountMinor: amount}, err
}
