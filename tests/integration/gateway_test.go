package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taseen007/MethodSplit/internal/config"
	"github.com/Taseen007/MethodSplit/internal/gateway"
	"github.com/Taseen007/MethodSplit/internal/middleware"
	"github.com/Taseen007/MethodSplit/internal/payments"
	"github.com/Taseen007/MethodSplit/internal/redisx"
)

func redisAddr() string {
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		return v
	}
	return "localhost:6379"
}

func gatewayRoutes() []config.Route {
	return []config.Route{
		{Method: "POST", Path: "/v1/payments", Class: config.ClassWrite, Idempotency: config.IdempotencyRequired},
		{Method: "POST", Path: "/v1/payments/{id}/execute", Class: config.ClassWrite, Idempotency: config.IdempotencyRequired},
		{Method: "POST", Path: "/v1/payments/{id}/refund", Class: config.ClassWrite, Idempotency: config.IdempotencyRequired},
		{Method: "GET", Path: "/v1/payments/{id}", Class: config.ClassRead, Consistency: config.ConsistencyStrong},
		{Method: "GET", Path: "/v1/payments", Class: config.ClassRead, Consistency: config.ConsistencyEventual, CacheTTL: config.Duration(10 * time.Second)},
		{Method: "GET", Path: "/v1/balance", Class: config.ClassRead, Consistency: config.ConsistencyEventual},
	}
}

// newGatewayTestServer wires a real Gateway to real (in-process, httptest)
// write/read backends sharing one Postgres, and real Redis. Both pool
// "targets" are backed by the same primary DB in this test setup — it
// exercises the gateway's own routing/auth/idempotency/cache/RYW logic,
// not cross-instance replication (that's covered separately against the
// real Postgres streaming replica, see manual verification in ARCHITECTURE.md).
func newGatewayTestServer(t *testing.T, mutate func(*config.Gateway)) (gwURL string, merchant config.Merchant, pool *pgxpool.Pool) {
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
	writeSrv := httptest.NewServer(payments.NewHandler(store, payments.ModeWrite).Routes())
	t.Cleanup(writeSrv.Close)
	readSrv := httptest.NewServer(payments.NewHandler(store, payments.ModeRead).Routes()) // same DB in this test; mirrors the real read-mode instance's API surface
	t.Cleanup(readSrv.Close)

	redisClient := redisx.New(redisAddr())
	t.Cleanup(func() { _ = redisClient.Close() })
	if err := redisClient.Ping(ctx); err != nil {
		t.Fatalf("ping redis: %v", err)
	}

	merchantID := "gwit_" + testRunID + "_" + sanitizeName(t.Name())
	merchant = config.Merchant{MerchantID: merchantID, APIKey: "key_" + merchantID, Secret: "secret_" + merchantID}
	ensureMerchant(t, pool, merchantID)

	cfg := &config.Gateway{
		SplitEnabled: true,
		Pools: config.Pools{
			Write: config.Pool{Targets: []string{writeSrv.URL}},
			Read:  config.Pool{Targets: []string{readSrv.URL}},
		},
		Routes: gatewayRoutes(),
		RateLimits: config.RateLimits{
			PerIPPerMin: 100000, PerMerchantReadPerMin: 100000, PerMerchantWritePerMin: 100000,
		},
		ReadYourWrites: config.ReadYourWrites{TTL: config.Duration(2 * time.Second)},
		Timeouts:       config.Timeouts{Upstream: config.Duration(5 * time.Second)},
		HealthCheck: config.HealthCheck{
			Path: "/healthz", Interval: config.Duration(time.Hour), Timeout: config.Duration(time.Second),
			UnhealthyAfter: 2, HealthyAfter: 2,
		},
		ML: config.ML{Mode: config.MLModeOff},
	}
	if mutate != nil {
		mutate(cfg)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := gateway.New(cfg, &config.Merchants{Merchants: []config.Merchant{merchant}}, redisClient, logger)
	if err != nil {
		t.Fatalf("build gateway: %v", err)
	}
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	return gwSrv.URL, merchant, pool
}

func sanitizeName(s string) string {
	return strings.NewReplacer("/", "_", " ", "_").Replace(s)
}

func signedDo(t *testing.T, merchant config.Merchant, method, gwURL, path, body, idemKey string) (*http.Response, []byte) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := middleware.BodyHash([]byte(body))
	canonical := middleware.CanonicalString(method, path, timestamp, bodyHash)
	sig := middleware.Sign(merchant.Secret, canonical)

	req, err := http.NewRequest(method, gwURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", merchant.APIKey)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", sig)
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
		t.Fatalf("read body: %v", err)
	}
	return resp, respBody
}

func TestGateway_AuthRequired(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, nil)

	resp, _ := signedDo(t, config.Merchant{MerchantID: "x", APIKey: "bad-key", Secret: "bad-secret"}, http.MethodGet, gwURL, "/v1/balance", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown api key: expected 401, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, gwURL+"/v1/balance", nil)
	req.Header.Set("X-Api-Key", merchant.APIKey)
	req.Header.Set("X-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Signature", "not-a-real-signature")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad signature: expected 401, got %d", resp2.StatusCode)
	}

	// Missing headers entirely.
	req3, _ := http.NewRequest(http.MethodGet, gwURL+"/v1/balance", nil)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing auth headers: expected 401, got %d", resp3.StatusCode)
	}

	// A timestamp well outside the 300s window, correctly signed for that
	// (old) timestamp — the staleness check must still reject it.
	oldTimestamp := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	canonical := middleware.CanonicalString(http.MethodGet, "/v1/balance", oldTimestamp, middleware.BodyHash(nil))
	req4, _ := http.NewRequest(http.MethodGet, gwURL+"/v1/balance", nil)
	req4.Header.Set("X-Api-Key", merchant.APIKey)
	req4.Header.Set("X-Timestamp", oldTimestamp)
	req4.Header.Set("X-Signature", middleware.Sign(merchant.Secret, canonical))
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Errorf("old timestamp: expected 401, got %d", resp4.StatusCode)
	}
}

// TestGateway_SpoofedMerchantIDIgnored is ARCHITECTURE.md §6's rule: on
// success, the gateway removes any client-sent X-Merchant-ID and sets its
// own. A client can't get its request attributed to a different merchant
// by sending that header itself.
func TestGateway_SpoofedMerchantIDIgnored(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, nil)

	body := `{"amount_minor":777,"currency":"BDT"}`
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	canonical := middleware.CanonicalString(http.MethodPost, "/v1/payments", timestamp, middleware.BodyHash([]byte(body)))
	req, err := http.NewRequest(http.MethodPost, gwURL+"/v1/payments", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", merchant.APIKey)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", middleware.Sign(merchant.Secret, canonical))
	req.Header.Set("Idempotency-Key", "spoof-check-1")
	req.Header.Set("X-Merchant-ID", "someone-else-entirely") // spoofed, must be ignored

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, respBody)
	}

	var payment struct {
		MerchantID string `json:"merchant_id"`
	}
	if err := json.Unmarshal(respBody, &payment); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payment.MerchantID != merchant.MerchantID {
		t.Errorf("expected the payment to be attributed to the authenticated merchant %q, got %q (spoofed X-Merchant-ID was not ignored)", merchant.MerchantID, payment.MerchantID)
	}
}

func TestGateway_UnknownPathAnd405(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, nil)

	resp, _ := signedDo(t, merchant, http.MethodGet, gwURL, "/v1/nope", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: expected 404, got %d", resp.StatusCode)
	}

	resp, _ = signedDo(t, merchant, http.MethodDelete, gwURL, "/v1/payments", "", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: expected 405, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("expected Allow header to include POST, got %q", allow)
	}
}

func TestGateway_CreateThroughGateway_IdempotentReplay(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, nil)

	resp, body := signedDo(t, merchant, http.MethodPost, gwURL, "/v1/payments", `{"amount_minor":4000,"currency":"BDT"}`, "gw-idem-1")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-MethodSplit-Pool") != gateway.PoolWrite {
		t.Errorf("expected X-MethodSplit-Pool: write, got %q", resp.Header.Get("X-MethodSplit-Pool"))
	}
	firstBody := string(body)

	resp2, body2 := signedDo(t, merchant, http.MethodPost, gwURL, "/v1/payments", `{"amount_minor":4000,"currency":"BDT"}`, "gw-idem-1")
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replay: expected 201, got %d: %s", resp2.StatusCode, body2)
	}
	if resp2.Header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("expected Idempotency-Replayed: true on the duplicate request")
	}
	if string(body2) != firstBody {
		t.Errorf("expected replay to return the same body, got different: %s vs %s", firstBody, body2)
	}

	resp3, _ := signedDo(t, merchant, http.MethodPost, gwURL, "/v1/payments", `{"amount_minor":9999,"currency":"BDT"}`, "gw-idem-1")
	if resp3.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("reused key with different body: expected 422, got %d", resp3.StatusCode)
	}

	resp4, _ := signedDo(t, merchant, http.MethodPost, gwURL, "/v1/payments", `{"amount_minor":1,"currency":"BDT"}`, "")
	if resp4.StatusCode != http.StatusBadRequest {
		t.Errorf("missing idempotency key: expected 400, got %d", resp4.StatusCode)
	}
}

func TestGateway_ReadYourWrites(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, nil) // 2s RYW TTL

	// A unique key per invocation: a fixed literal would replay on a repeat
	// run within the same process (go test -count>1 shares testRunID), and
	// a replayed write intentionally skips re-marking RYW (nothing new was
	// actually written), which would make this assertion fail for reasons
	// unrelated to RYW itself.
	idemKey := fmt.Sprintf("gw-ryw-%d", time.Now().UnixNano())
	resp, body := signedDo(t, merchant, http.MethodPost, gwURL, "/v1/payments", `{"amount_minor":1500,"currency":"BDT"}`, idemKey)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}

	respImmediate, _ := signedDo(t, merchant, http.MethodGet, gwURL, "/v1/balance", "", "")
	if pool := respImmediate.Header.Get("X-MethodSplit-Pool"); pool != gateway.PoolWrite {
		t.Errorf("immediately after a write, expected eventual read to route to the write pool (RYW active), got %q", pool)
	}

	time.Sleep(5 * time.Second) // RYW TTL is 2s; generous margin against scheduling jitter on a loaded machine

	respLater, _ := signedDo(t, merchant, http.MethodGet, gwURL, "/v1/balance", "", "")
	if pool := respLater.Header.Get("X-MethodSplit-Pool"); pool != gateway.PoolRead {
		t.Errorf("after the RYW TTL expires, expected eventual read to route to the read pool, got %q", pool)
	}
}

func TestGateway_CacheHitAfterMiss(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, nil)

	resp1, _ := signedDo(t, merchant, http.MethodGet, gwURL, "/v1/payments", "", "")
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp1.StatusCode)
	}
	if got := resp1.Header.Get("X-Cache"); got != "MISS" {
		t.Errorf("expected first request to be a cache MISS, got %q", got)
	}

	resp2, _ := signedDo(t, merchant, http.MethodGet, gwURL, "/v1/payments", "", "")
	if got := resp2.Header.Get("X-Cache"); got != "HIT" {
		t.Errorf("expected second identical request to be a cache HIT, got %q", got)
	}

	// A write must invalidate the cache (cachever bump) so stale data isn't served.
	invalidateKey := fmt.Sprintf("gw-cache-invalidate-%d", time.Now().UnixNano())
	respWrite, body := signedDo(t, merchant, http.MethodPost, gwURL, "/v1/payments", `{"amount_minor":333,"currency":"BDT"}`, invalidateKey)
	if respWrite.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", respWrite.StatusCode, body)
	}
	time.Sleep(5 * time.Second) // let the RYW window from this write pass so the next read can hit the read pool/cache path

	resp3, _ := signedDo(t, merchant, http.MethodGet, gwURL, "/v1/payments", "", "")
	if got := resp3.Header.Get("X-Cache"); got != "MISS" {
		t.Errorf("expected cache to miss after a write bumped the version, got %q", got)
	}
}

func TestGateway_RateLimited429(t *testing.T) {
	gwURL, merchant, _ := newGatewayTestServer(t, func(cfg *config.Gateway) {
		cfg.RateLimits.PerMerchantReadPerMin = 2
	})

	var last *http.Response
	for i := 0; i < 4; i++ {
		last, _ = signedDo(t, merchant, http.MethodGet, gwURL, "/v1/balance", "", "")
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the 4th request against a limit of 2/min to be 429, got %d", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on a 429")
	}
}
