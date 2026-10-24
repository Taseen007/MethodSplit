package ml

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Taseen007/MethodSplit/internal/gateway"
	"github.com/Taseen007/MethodSplit/internal/redisx"
)

const tightenedTTL = 60 * time.Second

// Guard implements gateway.MLGuard: it reads/updates the Redis feature
// windows, scores requests with the Isolation Forest (when a model is
// loaded and mode != off), logs one JSONL line per request, and applies
// the enforce-mode rate-limit cut for flagged merchants.
type Guard struct {
	mode     string
	features *FeatureStore
	model    *Model // nil until a model is loaded; Score is a no-op until then
	logger   *FeatureLogger
	redis    *redis.Client
	log      *slog.Logger
}

func NewGuard(mode string, redisClient *redisx.Client, logger *FeatureLogger, log *slog.Logger) *Guard {
	return &Guard{
		mode:     mode,
		features: NewFeatureStore(redisClient),
		logger:   logger,
		redis:    redisClient.Raw(),
		log:      log,
	}
}

// SetModel swaps in a (re)loaded model, e.g. on SIGHUP.
func (g *Guard) SetModel(m *Model) {
	g.model = m
}

func (g *Guard) Mode() string { return g.mode }

func (g *Guard) Score(ctx context.Context, apiKey string) (float64, bool, error) {
	if g.model == nil {
		return 0, false, nil
	}
	f, err := g.features.Read(ctx, apiKey)
	if err != nil {
		return 0, false, err
	}
	x := toFloat32Vector(f.Vector())
	score := g.model.ScoreSamples(x)
	return score, score < g.model.Threshold, nil
}

func (g *Guard) RecordAuthFailure(ctx context.Context, apiKey string) {
	if err := g.features.RecordAuthFailure(ctx, apiKey); err != nil {
		g.log.Warn("ml: record auth failure", "error", err)
	}
}

// Record logs the feature snapshot that was in effect for this request
// (read before RecordRequest folds this request's own outcome into the
// window) alongside its final status and score — called for every request
// regardless of mode, so ml.mode: off still collects training data.
func (g *Guard) Record(ctx context.Context, in gateway.RecordInput) {
	features, err := g.features.Read(ctx, in.APIKey)
	if err != nil {
		g.log.Warn("ml: read features for logging", "error", err)
	}

	if err := g.features.RecordRequest(ctx, in.APIKey, in.IsWrite, in.StatusCode, in.PaymentID); err != nil {
		g.log.Warn("ml: record request", "error", err)
	}

	if g.logger == nil {
		return
	}
	if err := g.logger.Log(FeatureLogRecord{
		Timestamp:      time.Now(),
		RequestID:      in.RequestID,
		APIKey:         in.APIKey,
		Route:          in.Route,
		ReqCount:       features.ReqCount,
		WriteRatio:     features.WriteRatio,
		ErrorRatio:     features.ErrorRatio,
		AuthFailCount:  features.AuthFailCount,
		DistinctIDs:    features.DistinctIDs,
		InterarrivalCV: features.InterarrivalCV,
		Status:         in.StatusCode,
		Score:          in.Score,
		Flagged:        in.Flagged,
	}); err != nil {
		g.log.Warn("ml: write feature log", "error", err)
	}
}

func (g *Guard) IsTightened(ctx context.Context, merchantID string) (bool, error) {
	n, err := g.redis.Exists(ctx, tightenedKey(merchantID)).Result()
	return n > 0, err
}

func (g *Guard) Tighten(ctx context.Context, merchantID string) error {
	return g.redis.Set(ctx, tightenedKey(merchantID), "1", tightenedTTL).Err()
}

func tightenedKey(merchantID string) string {
	return "ml:tightened:" + merchantID
}

func toFloat32Vector(v [6]float64) [6]float32 {
	var out [6]float32
	for i, val := range v {
		out[i] = float32(val)
	}
	return out
}
