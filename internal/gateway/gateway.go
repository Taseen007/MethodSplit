// Package gateway implements the MethodSplit reverse proxy: route
// matching, pools with health-checked round robin, HMAC auth, Redis-backed
// rate limiting/idempotency/caching/read-your-writes, and the ML guard —
// wired together in the request pipeline described in ARCHITECTURE.md §4.
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Taseen007/MethodSplit/internal/config"
	"github.com/Taseen007/MethodSplit/internal/metrics"
	"github.com/Taseen007/MethodSplit/internal/middleware"
	"github.com/Taseen007/MethodSplit/internal/redisx"
)

const (
	maxBodyBytes             = 1 << 20 // 1 MB, ARCHITECTURE.md §14 security checklist
	idempotencyProcessingTTL = 30 * time.Second
	idempotencyCompletedTTL  = 24 * time.Hour
	maxCacheableBodyBytes    = 256 * 1024
	authTimestampWindow      = 300 * time.Second
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var globallyAllowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true,
}

type Gateway struct {
	cfg            *config.Gateway
	merchantsByKey map[string]config.Merchant

	routes *RouteTable
	pools  map[string]*Pool

	proxier *Proxier

	redis       *redisx.Client
	rateLimiter *redisx.RateLimiter
	idempotency *redisx.Idempotency
	cache       *redisx.Cache
	ryw         *redisx.RYW

	ml MLGuard

	logger *slog.Logger
}

func New(cfg *config.Gateway, merchants *config.Merchants, redisClient *redisx.Client, logger *slog.Logger) (*Gateway, error) {
	readPool, err := NewPool(PoolRead, cfg.Pools.Read.Targets)
	if err != nil {
		return nil, err
	}
	writePool, err := NewPool(PoolWrite, cfg.Pools.Write.Targets)
	if err != nil {
		return nil, err
	}

	g := &Gateway{
		cfg:            cfg,
		merchantsByKey: merchants.ByAPIKey(),
		routes:         NewRouteTable(cfg.Routes),
		pools: map[string]*Pool{
			PoolRead:  readPool,
			PoolWrite: writePool,
		},
		proxier:     NewProxier(cfg.Timeouts.Upstream.Duration()),
		redis:       redisClient,
		rateLimiter: redisx.NewRateLimiter(redisClient),
		idempotency: redisx.NewIdempotency(redisClient),
		cache:       redisx.NewCache(redisClient),
		ryw:         redisx.NewRYW(redisClient),
		ml:          NoOpMLGuard{},
		logger:      logger,
	}
	return g, nil
}

// SetMLGuard swaps in a real ML guard (internal/ml) once one is loaded.
// Defaults to NoOpMLGuard so the gateway runs fine with ml.mode: off.
func (g *Gateway) SetMLGuard(ml MLGuard) {
	if ml != nil {
		g.ml = ml
	}
}

func (g *Gateway) Pools() map[string]*Pool {
	return g.pools
}

// respState carries the outcome of the request through to the access-log
// deferred call at the end of ServeHTTP.
type respState struct {
	status    int
	pool      string
	class     string
	mlScore   float64
	mlFlagged bool
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rs := &respState{status: http.StatusOK}

	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = middleware.NewRequestID()
	}
	w.Header().Set("X-Request-ID", requestID)

	// Populated once auth and routing succeed; used by the ML feature-record
	// call in the defer below (step 10 of ARCHITECTURE.md §4).
	var (
		recordAPIKey  string
		recordRoute   string
		recordIsWrite bool
		recordPayID   string
	)

	defer func() {
		if rec := recover(); rec != nil {
			g.logger.Error("panic recovered", "error", rec, "request_id", requestID)
			if rs.status == http.StatusOK {
				g.writeError(w, rs, requestID, http.StatusInternalServerError, "internal_error", "unexpected server error")
			}
		}
		metrics.RequestsTotal.WithLabelValues(r.Method, rs.class, rs.pool, strconv.Itoa(rs.status)).Inc()
		metrics.RequestDuration.WithLabelValues(rs.class, rs.pool).Observe(time.Since(start).Seconds())
		g.logger.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", rs.status,
			"class", rs.class, "pool", rs.pool,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", requestID,
		)
		if recordAPIKey != "" {
			g.ml.Record(r.Context(), RecordInput{
				APIKey: recordAPIKey, Route: recordRoute, RequestID: requestID,
				IsWrite: recordIsWrite, StatusCode: rs.status, PaymentID: recordPayID,
				Score: rs.mlScore, Flagged: rs.mlFlagged,
			})
		}
	}()

	// 2. Body size limit + buffer body.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		g.writeError(w, rs, requestID, http.StatusBadRequest, "invalid_body", "could not read request body")
		return
	}
	if len(body) > maxBodyBytes {
		g.writeError(w, rs, requestID, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MB")
		return
	}

	// 3. Method allowed at all? OPTIONS is answered here (CORS preflight).
	if r.Method == http.MethodOptions {
		g.writeCORSPreflight(w)
		rs.status = http.StatusNoContent
		return
	}
	if !globallyAllowedMethods[r.Method] {
		g.writeError(w, rs, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "unsupported HTTP method")
		return
	}

	// 4. Per-IP rate limit, before auth (cheap checks first).
	clientIP := clientIPFrom(r)
	ipResult, err := g.rateLimiter.Allow(r.Context(), "rl:ip:"+clientIP, g.cfg.RateLimits.PerIPPerMin, time.Minute)
	if err != nil {
		metrics.RedisFailOpenTotal.Inc()
		g.logger.Warn("redis unavailable for per-ip rate limit; failing open", "error", err, "request_id", requestID)
	} else if !ipResult.Allowed {
		metrics.RateLimitRejectedTotal.WithLabelValues("ip").Inc()
		g.writeRateLimited(w, rs, requestID, ipResult)
		return
	}

	// 5. HMAC auth -> merchant_id.
	merchant, apiKeyTried, authErr := g.authenticate(r, body)
	if authErr != "" {
		if apiKeyTried != "" {
			g.ml.RecordAuthFailure(r.Context(), apiKeyTried)
			// Still record this request (ARCHITECTURE.md §12.1's
			// credential-stuffing detection depends on auth failures
			// actually showing up in the training data, not just the
			// requests that got past auth).
			recordAPIKey = apiKeyTried
			recordRoute = r.Method + " " + r.URL.Path
		}
		g.writeError(w, rs, requestID, http.StatusUnauthorized, "unauthorized", authErr)
		return
	}
	r.Header.Del("X-Merchant-ID") // never trust a client-sent value
	r.Header.Set("X-Merchant-ID", merchant.MerchantID)

	// 6. Route match: method + path.
	match := g.routes.Match(r.Method, r.URL.Path)
	if !match.PathExists {
		g.writeError(w, rs, requestID, http.StatusNotFound, "not_found", "unknown path")
		return
	}
	if !match.Found {
		w.Header().Set("Allow", strings.Join(match.Allowed, ", "))
		g.writeError(w, rs, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this path")
		return
	}
	route := match.Route
	rs.class = route.Class
	recordAPIKey = merchant.APIKey
	recordRoute = route.Method + " " + r.URL.Path
	recordIsWrite = route.Class == config.ClassWrite
	recordPayID = match.Params["id"]

	// 7. Per-merchant rate limit by class.
	if blocked := g.checkMerchantRateLimit(w, rs, r, requestID, merchant.MerchantID, route); blocked {
		return
	}

	// 8. ML score: off / shadow / enforce.
	g.applyMLGuard(r, rs, merchant, route.Class == config.ClassWrite)

	extraHeaders := http.Header{}
	extraHeaders.Set("X-Merchant-ID", merchant.MerchantID)
	extraHeaders.Set("X-Forwarded-For", clientIP)
	extraHeaders.Set("X-Request-ID", requestID)

	if route.Class == config.ClassWrite {
		g.handleWrite(w, rs, r, requestID, body, route, merchant, extraHeaders)
	} else {
		g.handleRead(w, rs, r, requestID, body, route, merchant, extraHeaders)
	}
}

// authenticate returns the resolved merchant, the raw X-Api-Key that was
// tried (even on failure, so the caller can attribute the failure to it for
// the ML guard), and a non-empty error message on failure.
func (g *Gateway) authenticate(r *http.Request, body []byte) (config.Merchant, string, string) {
	apiKey := r.Header.Get("X-Api-Key")
	timestamp := r.Header.Get("X-Timestamp")
	signature := r.Header.Get("X-Signature")
	if apiKey == "" || timestamp == "" || signature == "" {
		return config.Merchant{}, apiKey, "missing X-Api-Key, X-Timestamp, or X-Signature header"
	}

	merchant, ok := g.merchantsByKey[apiKey]
	if !ok {
		return config.Merchant{}, apiKey, "unknown api key"
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return config.Merchant{}, apiKey, "invalid timestamp"
	}
	requestTime := time.Unix(ts, 0)
	drift := time.Since(requestTime)
	if drift < 0 {
		drift = -drift
	}
	if drift > authTimestampWindow {
		return config.Merchant{}, apiKey, "timestamp outside the allowed window"
	}

	pathWithQuery := r.URL.Path
	if r.URL.RawQuery != "" {
		pathWithQuery += "?" + r.URL.RawQuery
	}
	canonical := middleware.CanonicalString(r.Method, pathWithQuery, timestamp, middleware.BodyHash(body))
	if !middleware.VerifySignature(merchant.Secret, canonical, signature) {
		return config.Merchant{}, apiKey, "invalid signature"
	}

	return merchant, apiKey, ""
}

// enforceTightenDivisor is the rate-limit cut applied to a flagged
// merchant's requests in ml.mode: enforce (ARCHITECTURE.md §12.6: "cut to
// ¼ for 60s").
const enforceTightenDivisor = 4

// checkMerchantRateLimit applies the per-merchant limit for route.Class,
// cut to 1/4 if this merchant is currently under an enforce-mode ML
// tightening. On a Redis error it fails open for reads (best effort) and
// fails closed for writes (ARCHITECTURE.md §10: without Redis the gateway
// can't enforce idempotency or rate limits on writes, so it pauses them
// rather than risk a duplicate).
func (g *Gateway) checkMerchantRateLimit(w http.ResponseWriter, rs *respState, r *http.Request, requestID, merchantID string, route Route) bool {
	var key string
	var limit int
	var limitLabel string
	if route.Class == config.ClassWrite {
		key, limit, limitLabel = fmt.Sprintf("rl:m:%s:write", merchantID), g.cfg.RateLimits.PerMerchantWritePerMin, "write"
	} else {
		key, limit, limitLabel = fmt.Sprintf("rl:m:%s:read", merchantID), g.cfg.RateLimits.PerMerchantReadPerMin, "read"
	}

	tightened := false
	if g.ml.Mode() == config.MLModeEnforce {
		var err error
		tightened, err = g.ml.IsTightened(r.Context(), merchantID)
		if err != nil {
			g.logger.Warn("ml guard tightened-check failed", "error", err)
			tightened = false
		}
		if tightened {
			limit = max(1, limit/enforceTightenDivisor)
		}
	}

	result, err := g.rateLimiter.Allow(r.Context(), key, limit, time.Minute)
	if err != nil {
		metrics.RedisFailOpenTotal.Inc()
		if route.Class == config.ClassWrite {
			g.writeError(w, rs, requestID, http.StatusServiceUnavailable, "redis_unavailable", "cannot safely process writes without Redis")
			return true
		}
		g.logger.Warn("redis unavailable for per-merchant rate limit; failing open", "error", err, "request_id", requestID)
		return false
	}
	if !result.Allowed {
		metrics.RateLimitRejectedTotal.WithLabelValues(limitLabel).Inc()
		if tightened {
			w.Header().Set("X-MethodSplit-Reason", "anomaly")
		}
		g.writeRateLimited(w, rs, requestID, result)
		return true
	}
	return false
}

// applyMLGuard scores this request's API key (skipped entirely in
// ml.mode: off) and, in enforce mode, tightens the merchant's rate limits
// for subsequent requests if flagged.
func (g *Gateway) applyMLGuard(r *http.Request, rs *respState, merchant config.Merchant, isWrite bool) {
	if g.ml.Mode() == config.MLModeOff {
		return
	}
	score, flagged, err := g.ml.Score(r.Context(), merchant.APIKey)
	if err != nil {
		g.logger.Warn("ml guard score failed", "error", err)
		return
	}
	rs.mlScore, rs.mlFlagged = score, flagged
	metrics.MLScore.Observe(score)
	if !flagged {
		return
	}
	metrics.MLFlaggedTotal.WithLabelValues(g.ml.Mode()).Inc()
	if g.ml.Mode() == config.MLModeEnforce {
		if err := g.ml.Tighten(r.Context(), merchant.MerchantID); err != nil {
			g.logger.Warn("ml guard tighten failed", "error", err)
		}
	}
}

func clientIPFrom(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *Gateway) writeCORSPreflight(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Api-Key, X-Timestamp, X-Signature, Idempotency-Key")
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) writeRateLimited(w http.ResponseWriter, rs *respState, requestID string, result redisx.RateLimitResult) {
	remaining := result.Limit - result.Count
	if remaining < 0 {
		remaining = 0
	}
	w.Header().Set("RateLimit-Limit", strconv.FormatInt(result.Limit, 10))
	w.Header().Set("RateLimit-Remaining", strconv.FormatInt(remaining, 10))
	retryAfterSeconds := int(result.RetryAfter.Seconds())
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	g.writeError(w, rs, requestID, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
}

func (g *Gateway) writeError(w http.ResponseWriter, rs *respState, requestID string, status int, code, message string) {
	rs.status = status
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: message, RequestID: requestID}})
}

func (g *Gateway) writeUpstream(w http.ResponseWriter, rs *respState, result ProxyResult, pool string, extra map[string]string) {
	rs.status = result.StatusCode
	rs.pool = pool
	h := w.Header()
	if ct := result.Header.Get("Content-Type"); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	h.Set("X-MethodSplit-Pool", pool)
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}
