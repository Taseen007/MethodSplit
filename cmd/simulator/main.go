// Command simulator generates labeled traffic for the ML anomaly guard
// (ARCHITECTURE.md §12.3): normal merchant profiles plus several attack
// profiles, each on its own API key so labels come from the key->profile
// mapping instead of manual labeling.
//
// It seeds the profiles' merchants directly into Postgres and writes a
// merchants.yaml covering them (merge this with configs/merchants.yaml — or
// point the gateway straight at the generated file — before running the
// gateway that will serve this traffic), then drives HMAC-signed requests
// at the gateway according to each profile's behavior pattern.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/Taseen007/MethodSplit/internal/middleware"
)

type Profile string

const (
	ProfileNormal             Profile = "normal"
	ProfileBurstFlood         Profile = "burst_flood"
	ProfileLowAndSlowBot      Profile = "low_and_slow_bot"
	ProfileIDEnumeration      Profile = "id_enumeration"
	ProfileCredentialStuffing Profile = "credential_stuffing"
	ProfileRetryStorm         Profile = "retry_storm"
)

var allProfiles = []Profile{
	ProfileNormal, ProfileNormal, ProfileNormal, // several normal merchants
	ProfileBurstFlood,
	ProfileLowAndSlowBot,
	ProfileIDEnumeration,
	ProfileCredentialStuffing,
	ProfileRetryStorm,
}

type simMerchant struct {
	MerchantID string  `yaml:"merchant_id"`
	APIKey     string  `yaml:"api_key"`
	Secret     string  `yaml:"secret"`
	Profile    Profile `yaml:"-"`
}

type merchantsYAML struct {
	Merchants []struct {
		MerchantID string `yaml:"merchant_id"`
		APIKey     string `yaml:"api_key"`
		Secret     string `yaml:"secret"`
	} `yaml:"merchants"`
}

func main() {
	var (
		gatewayURL   = flag.String("gateway", "http://localhost:8080", "gateway base URL")
		databaseURL  = flag.String("db", os.Getenv("DATABASE_URL"), "Postgres DSN to seed simulated merchants into")
		merchantsOut = flag.String("merchants-out", "ml/data/sim_merchants.yaml", "where to write the generated merchants.yaml")
		labelsOut    = flag.String("labels-out", "ml/data/labels.json", "where to write the api_key -> profile label map")
		seed         = flag.Int64("seed", 1, "PRNG seed (use a different seed for train vs. test, per ARCHITECTURE.md §12.3)")
		duration     = flag.Duration("duration", 60*time.Second, "how long to run traffic")
		intensity    = flag.Float64("intensity", 1.0, "attack intensity multiplier (use a different value for train vs. test)")
		skipTraffic  = flag.Bool("skip-traffic", false, "generate/seed merchants and exit, without driving traffic (use for a first pass before the gateway is configured with these merchants)")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	rng := rand.New(rand.NewPCG(uint64(*seed), uint64(*seed)>>1|1))

	merchants := generateMerchants(rng)

	if *databaseURL != "" {
		if err := seedMerchants(context.Background(), *databaseURL, merchants); err != nil {
			logger.Error("seed merchants", "error", err)
			os.Exit(1)
		}
		logger.Info("seeded merchants", "count", len(merchants))
	}

	if err := writeMerchantsYAML(*merchantsOut, merchants); err != nil {
		logger.Error("write merchants yaml", "error", err)
		os.Exit(1)
	}
	if err := writeLabels(*labelsOut, merchants); err != nil {
		logger.Error("write labels", "error", err)
		os.Exit(1)
	}
	logger.Info("wrote merchants + labels", "merchants_out", *merchantsOut, "labels_out", *labelsOut)

	if *skipTraffic {
		logger.Info("skip-traffic set; exiting after generating merchants")
		return
	}

	logger.Info("starting traffic", "duration", duration.String(), "intensity", *intensity, "seed", *seed)
	runTraffic(logger, *gatewayURL, merchants, *duration, *intensity, rng)
	logger.Info("done")
}

func generateMerchants(rng *rand.Rand) []simMerchant {
	out := make([]simMerchant, 0, len(allProfiles))
	for i, p := range allProfiles {
		out = append(out, simMerchant{
			MerchantID: fmt.Sprintf("sim_%s_%d", p, i),
			APIKey:     "sim_key_" + randomHex(rng, 8),
			Secret:     "sim_secret_" + randomHex(rng, 16),
			Profile:    p,
		})
	}
	return out
}

// randomHex is seeded-PRNG-only (not crypto/rand): the simulator's whole
// point is reproducibility for a given -seed, including the merchant IDs
// and keys it generates, so a second run (e.g. one pass to generate+seed
// merchants, a later pass to drive traffic against them) reproduces the
// same merchants instead of minting new ones each time.
func randomHex(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.IntN(256))
	}
	return hex.EncodeToString(b)
}

func seedMerchants(ctx context.Context, dsn string, merchants []simMerchant) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, m := range merchants {
		if _, err := pool.Exec(ctx, `
			INSERT INTO merchants (id, name) VALUES ($1, $1)
			ON CONFLICT (id) DO NOTHING
		`, m.MerchantID); err != nil {
			return fmt.Errorf("seed merchant %s: %w", m.MerchantID, err)
		}
	}
	return nil
}

func writeMerchantsYAML(path string, merchants []simMerchant) error {
	var doc merchantsYAML
	for _, m := range merchants {
		doc.Merchants = append(doc.Merchants, struct {
			MerchantID string `yaml:"merchant_id"`
			APIKey     string `yaml:"api_key"`
			Secret     string `yaml:"secret"`
		}{m.MerchantID, m.APIKey, m.Secret})
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeLabels(path string, merchants []simMerchant) error {
	labels := make(map[string]string, len(merchants))
	for _, m := range merchants {
		labels[m.APIKey] = string(m.Profile)
	}
	data, err := json.MarshalIndent(labels, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func runTraffic(logger *slog.Logger, gatewayURL string, merchants []simMerchant, duration time.Duration, intensity float64, rng *rand.Rand) {
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	for _, m := range merchants {
		wg.Add(1)
		// Each merchant's own PCG stream, seeded from the shared rng, so
		// concurrent goroutines don't share (and race on) one generator.
		merchantRng := rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64()))
		go func(m simMerchant) {
			defer wg.Done()
			driveProfile(logger, client, gatewayURL, m, deadline, intensity, merchantRng)
		}(m)
	}
	wg.Wait()
}

func driveProfile(logger *slog.Logger, client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, intensity float64, rng *rand.Rand) {
	switch m.Profile {
	case ProfileNormal:
		driveNormal(client, gatewayURL, m, deadline, rng)
	case ProfileBurstFlood:
		driveBurstFlood(client, gatewayURL, m, deadline, intensity, rng)
	case ProfileLowAndSlowBot:
		driveLowAndSlow(client, gatewayURL, m, deadline, rng)
	case ProfileIDEnumeration:
		driveIDEnumeration(client, gatewayURL, m, deadline, intensity, rng)
	case ProfileCredentialStuffing:
		driveCredentialStuffing(client, gatewayURL, m, deadline, intensity, rng)
	case ProfileRetryStorm:
		driveRetryStorm(client, gatewayURL, m, deadline, intensity, rng)
	default:
		logger.Warn("unknown profile", "profile", m.Profile)
	}
}

// driveNormal: occasional create -> execute -> status flow with Poisson-ish
// gaps, a few errors sprinkled in (e.g. checking a payment that doesn't exist).
func driveNormal(client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, rng *rand.Rand) {
	for time.Now().Before(deadline) {
		body := fmt.Sprintf(`{"amount_minor":%d,"currency":"BDT"}`, 1000+rng.IntN(9000))
		id := createPayment(client, gatewayURL, m, body, randomHex(rng, 6))
		sleepPoisson(rng, 2*time.Second)
		if id != "" && rng.Float64() < 0.7 {
			executePayment(client, gatewayURL, m, id, randomHex(rng, 6))
		}
		sleepPoisson(rng, 3*time.Second)
		if rng.Float64() < 0.1 {
			getPayment(client, gatewayURL, m, "not-a-real-id") // occasional 404, contributes to error_ratio
		}
	}
}

// driveBurstFlood: short bursts of many GETs, well above normal read rates.
func driveBurstFlood(client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, intensity float64, rng *rand.Rand) {
	burstSize := int(20 * intensity)
	for time.Now().Before(deadline) {
		for i := 0; i < burstSize; i++ {
			listPayments(client, gatewayURL, m)
		}
		time.Sleep(2 * time.Second)
	}
}

// driveLowAndSlow: stays just under a plausible rate limit, very regular
// timing (low interarrival_cv is the tell).
func driveLowAndSlow(client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, rng *rand.Rand) {
	for time.Now().Before(deadline) {
		listPayments(client, gatewayURL, m)
		time.Sleep(1500 * time.Millisecond) // fixed interval, not randomized
	}
}

// driveIDEnumeration: requests many different (mostly nonexistent) payment
// IDs — the distinct_ids feature is built for exactly this.
func driveIDEnumeration(client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, intensity float64, rng *rand.Rand) {
	rate := time.Duration(float64(200*time.Millisecond) / intensity)
	for time.Now().Before(deadline) {
		fakeID := randomHex(rng, 16)
		getPayment(client, gatewayURL, m, fakeID)
		time.Sleep(rate)
	}
}

// driveCredentialStuffing: repeated requests with a deliberately wrong
// signature — every one fails auth, driving auth_fail_count up.
func driveCredentialStuffing(client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, intensity float64, rng *rand.Rand) {
	rate := time.Duration(float64(150*time.Millisecond) / intensity)
	for time.Now().Before(deadline) {
		sendWithBadSignature(client, gatewayURL, m)
		time.Sleep(rate)
	}
}

// driveRetryStorm: the same create request repeated rapidly with the same
// Idempotency-Key — legitimate to retry, but at an abnormal rate.
func driveRetryStorm(client *http.Client, gatewayURL string, m simMerchant, deadline time.Time, intensity float64, rng *rand.Rand) {
	idemKey := "retry-storm-" + randomHex(rng, 6)
	body := `{"amount_minor":500,"currency":"BDT"}`
	rate := time.Duration(float64(100*time.Millisecond) / intensity)
	for time.Now().Before(deadline) {
		createPayment(client, gatewayURL, m, body, idemKey)
		time.Sleep(rate)
	}
}

func sleepPoisson(rng *rand.Rand, mean time.Duration) {
	u := rng.Float64()
	if u <= 0 {
		u = 0.0001
	}
	d := time.Duration(-float64(mean) * math.Log(u))
	if d > 10*mean {
		d = 10 * mean
	}
	time.Sleep(d)
}

func createPayment(client *http.Client, gatewayURL string, m simMerchant, body, idemKey string) string {
	resp, respBody := doSigned(client, gatewayURL, m, http.MethodPost, "/v1/payments", body, idemKey)
	if resp == nil || resp.StatusCode >= 300 {
		return ""
	}
	var parsed struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	return parsed.ID
}

func executePayment(client *http.Client, gatewayURL string, m simMerchant, id, idemKey string) {
	doSigned(client, gatewayURL, m, http.MethodPost, "/v1/payments/"+id+"/execute", "", idemKey)
}

func getPayment(client *http.Client, gatewayURL string, m simMerchant, id string) {
	doSigned(client, gatewayURL, m, http.MethodGet, "/v1/payments/"+id, "", "")
}

func listPayments(client *http.Client, gatewayURL string, m simMerchant) {
	doSigned(client, gatewayURL, m, http.MethodGet, "/v1/payments?limit=5", "", "")
}

func doSigned(client *http.Client, gatewayURL string, m simMerchant, method, path, body, idemKey string) (*http.Response, []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := middleware.BodyHash([]byte(body))
	canonical := middleware.CanonicalString(method, path, timestamp, bodyHash)
	signature := middleware.Sign(m.Secret, canonical)

	req, err := http.NewRequest(method, gatewayURL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", m.APIKey)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", signature)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody
}

func sendWithBadSignature(client *http.Client, gatewayURL string, m simMerchant) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequest(http.MethodGet, gatewayURL+"/v1/balance", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Api-Key", m.APIKey)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", "0000000000000000000000000000000000000000000000000000000000000000")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
