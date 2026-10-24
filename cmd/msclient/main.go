// Command msclient signs and sends a request to the gateway, since signing
// HMAC requests by hand is painful (ARCHITECTURE.md §6).
//
// Usage:
//
//	msclient -key mk_demo_a [-secret sk_...] [-base http://localhost:8080] \
//	  -X POST [-idem order-1] [-d '{"amount_minor":50000,"currency":"BDT"}'] /v1/payments
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Taseen007/MethodSplit/internal/config"
	"github.com/Taseen007/MethodSplit/internal/middleware"
)

func main() {
	var (
		apiKey    = flag.String("key", "", "merchant API key (required)")
		secret    = flag.String("secret", "", "merchant secret; looked up from -merchants if empty")
		merchants = flag.String("merchants", "configs/merchants.yaml", "path to merchants.yaml, used to look up -secret when not given directly")
		base      = flag.String("base", "http://localhost:8080", "gateway base URL")
		method    = flag.String("X", http.MethodGet, "HTTP method")
		idemKey   = flag.String("idem", "", "Idempotency-Key header (required for write routes)")
		data      = flag.String("d", "", "request body")
	)
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: msclient -key <api_key> [-X METHOD] [-idem KEY] [-d BODY] <path>")
		os.Exit(2)
	}
	path := flag.Arg(0)

	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: -key is required")
		os.Exit(2)
	}

	resolvedSecret := *secret
	if resolvedSecret == "" {
		m, err := config.LoadMerchants(*merchants)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: -secret not given and could not load %s: %v\n", *merchants, err)
			os.Exit(1)
		}
		merchant, ok := m.ByAPIKey()[*apiKey]
		if !ok {
			fmt.Fprintf(os.Stderr, "error: no merchant with api_key %q in %s\n", *apiKey, *merchants)
			os.Exit(1)
		}
		resolvedSecret = merchant.Secret
	}

	body := []byte(*data)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := middleware.BodyHash(body)
	canonical := middleware.CanonicalString(strings.ToUpper(*method), path, timestamp, bodyHash)
	signature := middleware.Sign(resolvedSecret, canonical)

	req, err := http.NewRequest(strings.ToUpper(*method), strings.TrimSuffix(*base, "/")+path, strings.NewReader(*data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: build request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", *apiKey)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", signature)
	if *idemKey != "" {
		req.Header.Set("Idempotency-Key", *idemKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read response: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "HTTP %s\n", resp.Status)
	for _, h := range []string{"X-Request-ID", "X-MethodSplit-Pool", "X-MethodSplit-Reason", "X-Cache", "Idempotency-Replayed", "RateLimit-Limit", "RateLimit-Remaining", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(os.Stderr, "%s: %s\n", h, v)
		}
	}
	fmt.Println(string(respBody))

	if resp.StatusCode >= 400 {
		os.Exit(1)
	}
}
