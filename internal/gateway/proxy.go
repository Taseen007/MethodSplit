package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

type ProxyResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// ConnError is true when the request never got a response at all
	// (dial/timeout/connection reset) as opposed to the upstream answering
	// with an HTTP status, even a 5xx one. Only ConnError is retryable
	// (ARCHITECTURE.md §5.5).
	ConnError bool
}

type Proxier struct {
	client *http.Client
}

func NewProxier(timeout time.Duration) *Proxier {
	return &Proxier{
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Do forwards the (already-buffered) request body to target, copying the
// incoming method/path/query and headers, then layering extraHeaders on top
// (used for the headers the gateway itself controls, like X-Merchant-ID).
func (p *Proxier) Do(ctx context.Context, r *http.Request, body []byte, target *Target, extraHeaders http.Header) ProxyResult {
	upstreamURL := *target.URL
	upstreamURL.Path = singleJoiningSlash(target.URL.Path, r.URL.Path)
	upstreamURL.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		return ProxyResult{ConnError: true}
	}
	req.Header = r.Header.Clone()
	stripHopByHopHeaders(req.Header)
	for k, vv := range extraHeaders {
		req.Header[k] = vv
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return ProxyResult{ConnError: true}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProxyResult{ConnError: true}
	}

	return ProxyResult{StatusCode: resp.StatusCode, Header: resp.Header, Body: respBody}
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
	"Te", "Trailer", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
}

func stripHopByHopHeaders(h http.Header) {
	for _, header := range hopByHopHeaders {
		h.Del(header)
	}
}
