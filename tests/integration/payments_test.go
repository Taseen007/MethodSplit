// Package integration holds tests that need a live Postgres and Redis
// (see Makefile's itest target). They're skipped under `go test -short`.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taseen007/MethodSplit/internal/payments"
)

func primaryDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PG_PRIMARY_DSN")
	if dsn == "" {
		dsn = "postgres://methodsplit:methodsplit_dev_password@localhost:5432/methodsplit?sslmode=disable"
	}
	return dsn
}

func newPaymentsServer(t *testing.T, mode string) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, primaryDSN(t))
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	store := payments.NewStore(pool)
	handler := payments.NewHandler(store, mode)
	srv := httptest.NewServer(handler.Routes())
	t.Cleanup(srv.Close)
	return srv, pool
}

// testRunID makes merchant IDs (and therefore idempotency records, which
// are keyed by merchant_id + key) unique per `go test` invocation. Without
// it, re-running the suite against the same persistent dev Postgres would
// replay idempotency records left over from an earlier run instead of
// exercising the test's sequence fresh.
var testRunID = strconv.FormatInt(time.Now().UnixNano(), 36)

func testMerchantID(t *testing.T) string {
	return "it_merchant_" + testRunID + "_" + strings.ReplaceAll(t.Name(), "/", "_")
}

func ensureMerchant(t *testing.T, pool *pgxpool.Pool, merchantID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO merchants (id, name) VALUES ($1, $1)
		ON CONFLICT (id) DO NOTHING
	`, merchantID)
	if err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
}

func doJSON(t *testing.T, method, url, merchantID, idemKey string, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if merchantID != "" {
		req.Header.Set("X-Merchant-ID", merchantID)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, respBody
}

// TestCreateExecuteRefundFlow exercises the full payment lifecycle and
// checks that balance reflects only currently-COMPLETED payments.
func TestCreateExecuteRefundFlow(t *testing.T) {
	srv, pool := newPaymentsServer(t, payments.ModeWrite)
	merchantID := testMerchantID(t)
	ensureMerchant(t, pool, merchantID)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/payments", merchantID, "flow-create", `{"amount_minor":5000,"currency":"BDT"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var created payments.Payment
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.Status != payments.StatusCreated {
		t.Fatalf("expected status CREATED, got %s", created.Status)
	}

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/payments/"+created.ID+"/execute", merchantID, "flow-execute", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("execute: expected 200, got %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, http.MethodGet, srv.URL+"/v1/balance", merchantID, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("balance: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var bal payments.Balance
	_ = json.Unmarshal(body, &bal)
	if bal.AmountMinor != 5000 {
		t.Fatalf("expected balance 5000 after execute, got %d", bal.AmountMinor)
	}

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/payments/"+created.ID+"/refund", merchantID, "flow-refund", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refund: expected 200, got %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, http.MethodGet, srv.URL+"/v1/balance", merchantID, "", "")
	_ = json.Unmarshal(body, &bal)
	if bal.AmountMinor != 0 {
		t.Fatalf("expected balance 0 after refund, got %d (this is the COMPLETED-minus-REFUNDED double-count bug if nonzero)", bal.AmountMinor)
	}

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/payments/"+created.ID+"/refund", merchantID, "flow-refund-again", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double refund: expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// TestIdempotency_ConcurrentDuplicateCreate is experiment E3: 100 concurrent
// identical POSTs with the same Idempotency-Key must create exactly one
// payment row.
func TestIdempotency_ConcurrentDuplicateCreate(t *testing.T) {
	srv, pool := newPaymentsServer(t, payments.ModeWrite)
	merchantID := testMerchantID(t)
	ensureMerchant(t, pool, merchantID)

	const n = 100
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/payments", merchantID, "e3-concurrent-key", `{"amount_minor":999,"currency":"BDT"}`)
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for _, s := range statuses {
		if s != http.StatusCreated {
			t.Fatalf("expected every concurrent request to return 201, got %d", s)
		}
	}

	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payments WHERE merchant_id = $1 AND amount_minor = 999`, merchantID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if count != 1 {
		t.Fatalf("E3: expected exactly 1 payment row from %d concurrent identical requests, got %d", n, count)
	}
}

func TestIdempotency_KeyReusedWithDifferentBody(t *testing.T) {
	srv, pool := newPaymentsServer(t, payments.ModeWrite)
	merchantID := testMerchantID(t)
	ensureMerchant(t, pool, merchantID)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/payments", merchantID, "mismatch-key", `{"amount_minor":100,"currency":"BDT"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/payments", merchantID, "mismatch-key", `{"amount_minor":200,"currency":"BDT"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("reused key with different body: expected 422, got %d: %s", resp.StatusCode, body)
	}
}

// TestGetPayment_MalformedID is a regression test: a non-UUID path segment
// (e.g. an ID-enumeration attempt guessing random strings, not just random
// UUIDs) must look like "not found" (404), not a raw database error (500).
func TestGetPayment_MalformedID(t *testing.T) {
	srv, pool := newPaymentsServer(t, payments.ModeWrite)
	merchantID := testMerchantID(t)
	ensureMerchant(t, pool, merchantID)

	resp, body := doJSON(t, http.MethodGet, srv.URL+"/v1/payments/not-a-real-id", merchantID, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("malformed id: expected 404, got %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/payments/not-a-real-id/execute", merchantID, "malformed-id-execute", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("malformed id on execute: expected 404, got %d: %s", resp.StatusCode, body)
	}
}

func TestReadMode_RejectsWrites(t *testing.T) {
	srv, _ := newPaymentsServer(t, payments.ModeRead)
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/payments", "someone", "k", `{"amount_minor":1,"currency":"BDT"}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST on a read-mode instance, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("expected Allow header to list GET, got %q", allow)
	}
}

func TestMain(m *testing.M) {
	fmt.Println("running payments integration tests")
	os.Exit(m.Run())
}
