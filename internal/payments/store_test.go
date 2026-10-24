package payments

import "testing"

func TestHashRequest(t *testing.T) {
	a := HashRequest("POST", "/v1/payments", []byte(`{"amount_minor":100}`))
	b := HashRequest("POST", "/v1/payments", []byte(`{"amount_minor":100}`))
	if a != b {
		t.Fatalf("expected identical inputs to hash the same, got %s != %s", a, b)
	}

	diffBody := HashRequest("POST", "/v1/payments", []byte(`{"amount_minor":200}`))
	if a == diffBody {
		t.Fatalf("expected different bodies to hash differently")
	}

	diffMethod := HashRequest("GET", "/v1/payments", []byte(`{"amount_minor":100}`))
	if a == diffMethod {
		t.Fatalf("expected different methods to hash differently")
	}

	diffPath := HashRequest("POST", "/v1/payments/123/execute", []byte(`{"amount_minor":100}`))
	if a == diffPath {
		t.Fatalf("expected different paths to hash differently")
	}

	// Concatenation without a separator would make "GET" + "X/a" collide with
	// "GE" + "TX/a"; the newline-separated hash must not do that.
	c1 := HashRequest("GET", "X/a", nil)
	c2 := HashRequest("GE", "TX/a", nil)
	if c1 == c2 {
		t.Fatalf("expected method/path boundary to be unambiguous in the hash")
	}
}
