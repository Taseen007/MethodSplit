package gateway

import (
	"net/http"
	"testing"

	"github.com/Taseen007/MethodSplit/internal/config"
)

func testRoutes() []config.Route {
	return []config.Route{
		{Method: "POST", Path: "/v1/payments", Class: config.ClassWrite, Idempotency: config.IdempotencyRequired},
		{Method: "POST", Path: "/v1/payments/{id}/execute", Class: config.ClassWrite, Idempotency: config.IdempotencyRequired},
		{Method: "POST", Path: "/v1/payments/{id}/refund", Class: config.ClassWrite, Idempotency: config.IdempotencyRequired},
		{Method: "GET", Path: "/v1/payments/{id}", Class: config.ClassRead, Consistency: config.ConsistencyStrong},
		{Method: "GET", Path: "/v1/payments", Class: config.ClassRead, Consistency: config.ConsistencyEventual, CacheTTL: config.Duration(10)},
		{Method: "GET", Path: "/v1/balance", Class: config.ClassRead, Consistency: config.ConsistencyEventual},
	}
}

func TestRouteTable_Match_Found(t *testing.T) {
	rt := NewRouteTable(testRoutes())

	m := rt.Match(http.MethodGet, "/v1/payments/abc-123")
	if !m.Found {
		t.Fatalf("expected match for GET /v1/payments/abc-123")
	}
	if m.Route.Consistency != config.ConsistencyStrong {
		t.Errorf("expected strong consistency, got %s", m.Route.Consistency)
	}
	if m.Params["id"] != "abc-123" {
		t.Errorf("expected id param abc-123, got %q", m.Params["id"])
	}

	m = rt.Match(http.MethodPost, "/v1/payments/abc-123/execute")
	if !m.Found {
		t.Fatalf("expected match for POST execute")
	}
	if !m.Route.IdempotencyRequired {
		t.Errorf("expected idempotency required on execute route")
	}
	if m.Params["id"] != "abc-123" {
		t.Errorf("expected id param abc-123, got %q", m.Params["id"])
	}
}

func TestRouteTable_Match_HeadFallsBackToGet(t *testing.T) {
	rt := NewRouteTable(testRoutes())
	m := rt.Match(http.MethodHead, "/v1/balance")
	if !m.Found {
		t.Fatalf("expected HEAD to match the GET /v1/balance route")
	}
	if m.Route.Class != config.ClassRead {
		t.Errorf("expected read class, got %s", m.Route.Class)
	}
}

func TestRouteTable_Match_WrongMethod405(t *testing.T) {
	rt := NewRouteTable(testRoutes())
	m := rt.Match(http.MethodDelete, "/v1/payments")
	if m.Found {
		t.Fatalf("expected no match for DELETE /v1/payments")
	}
	if !m.PathExists {
		t.Fatalf("expected path to exist for /v1/payments")
	}
	found := map[string]bool{}
	for _, a := range m.Allowed {
		found[a] = true
	}
	if !found["GET"] || !found["POST"] || !found["HEAD"] {
		t.Errorf("expected Allow to include GET, POST, HEAD; got %v", m.Allowed)
	}
}

func TestRouteTable_Match_UnknownPath404(t *testing.T) {
	rt := NewRouteTable(testRoutes())
	m := rt.Match(http.MethodGet, "/v1/nope")
	if m.Found || m.PathExists {
		t.Fatalf("expected no match at all for unknown path, got %+v", m)
	}
}

func TestRouteTable_Match_DoesNotConfuseSiblingSegments(t *testing.T) {
	rt := NewRouteTable(testRoutes())
	// /v1/payments/{id} (GET, strong) must not match the 3-segment
	// /v1/payments/{id}/execute path. That path exists (as POST-only), so
	// GET on it is a 405 with Allow: POST, not a 200 and not a 404.
	m := rt.Match(http.MethodGet, "/v1/payments/abc/execute")
	if m.Found {
		t.Fatalf("expected GET not to match the POST-only execute path, got %+v", m)
	}
	if !m.PathExists {
		t.Fatalf("expected the execute path to exist (as POST), got %+v", m)
	}
	if len(m.Allowed) != 1 || m.Allowed[0] != "POST" {
		t.Errorf("expected Allow: POST only, got %v", m.Allowed)
	}
}
