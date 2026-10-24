// Package ml implements the anomaly guard: per-API-key windowed features
// read from Redis (ARCHITECTURE.md §12.2), an Isolation Forest scorer
// ported from scikit-learn (§12.5), and the off/shadow/enforce guard that
// ties them together (§12.6).
package ml

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Taseen007/MethodSplit/internal/redisx"
)

const (
	bucketDuration = 10 * time.Second
	numBuckets     = 6 // 6 * 10s = the 60s feature window
	bucketTTL      = 70 * time.Second
	maxTimestamps  = 20
)

// Features are the six per-API-key signals scored by the Isolation Forest.
// Field order matches ml/train.py's feature column order; internal/ml/iforest.go
// depends on this order being stable.
type Features struct {
	ReqCount       float64
	WriteRatio     float64
	ErrorRatio     float64
	AuthFailCount  float64
	DistinctIDs    float64
	InterarrivalCV float64
}

// Vector returns the features as a fixed-order slice for the scorer.
func (f Features) Vector() [6]float64 {
	return [6]float64{f.ReqCount, f.WriteRatio, f.ErrorRatio, f.AuthFailCount, f.DistinctIDs, f.InterarrivalCV}
}

var FeatureNames = [6]string{"req_count", "write_ratio", "error_ratio", "auth_fail_count", "distinct_ids", "interarrival_cv"}

type FeatureStore struct {
	rdb *redis.Client
}

func NewFeatureStore(client *redisx.Client) *FeatureStore {
	return &FeatureStore{rdb: client.Raw()}
}

func currentBucket(t time.Time) int64 {
	return t.Unix() / int64(bucketDuration.Seconds())
}

func bucketKey(apiKey string, bucket int64) string {
	return fmt.Sprintf("feat:%s:%d", apiKey, bucket)
}

func timestampsKey(apiKey string) string {
	return "feat:" + apiKey + ":ts"
}

// RecordAuthFailure counts a failed signature against apiKey — including an
// unknown key, which is itself a signal (ARCHITECTURE.md §6: "Count failed
// signatures per X-Api-Key").
func (s *FeatureStore) RecordAuthFailure(ctx context.Context, apiKey string) error {
	key := bucketKey(apiKey, currentBucket(time.Now()))
	pipe := s.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, "authfail", 1)
	pipe.Expire(ctx, key, bucketTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// RecordRequest updates the feature windows after a request completes
// (ARCHITECTURE.md §12.2: "updated after the response, once the status is
// known"). paymentID is used for the distinct_ids HyperLogLog when non-empty.
func (s *FeatureStore) RecordRequest(ctx context.Context, apiKey string, isWrite bool, statusCode int, paymentID string) error {
	now := time.Now()
	key := bucketKey(apiKey, currentBucket(now))

	pipe := s.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, "req", 1)
	if isWrite {
		pipe.HIncrBy(ctx, key, "write", 1)
	}
	if statusCode >= 400 {
		pipe.HIncrBy(ctx, key, "err", 1)
	}
	pipe.Expire(ctx, key, bucketTTL)

	if paymentID != "" {
		idsKey := key + ":ids"
		pipe.PFAdd(ctx, idsKey, paymentID)
		pipe.Expire(ctx, idsKey, bucketTTL)
	}

	tsKey := timestampsKey(apiKey)
	pipe.LPush(ctx, tsKey, now.UnixMilli())
	pipe.LTrim(ctx, tsKey, 0, maxTimestamps-1)
	pipe.Expire(ctx, tsKey, bucketTTL)

	_, err := pipe.Exec(ctx)
	return err
}

// Read computes the current 60s-window features for apiKey with one
// pipelined read across the 6 buckets (ARCHITECTURE.md §12.2).
func (s *FeatureStore) Read(ctx context.Context, apiKey string) (Features, error) {
	now := currentBucket(time.Now())

	pipe := s.rdb.Pipeline()
	hcmds := make([]*redis.MapStringStringCmd, numBuckets)
	idKeys := make([]string, numBuckets)
	for i := 0; i < numBuckets; i++ {
		k := bucketKey(apiKey, now-int64(i))
		hcmds[i] = pipe.HGetAll(ctx, k)
		idKeys[i] = k + ":ids"
	}
	pfCountCmd := pipe.PFCount(ctx, idKeys...)
	tsCmd := pipe.LRange(ctx, timestampsKey(apiKey), 0, maxTimestamps-1)

	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return Features{}, err
	}

	var reqTotal, writeTotal, errTotal, authFailTotal float64
	for _, cmd := range hcmds {
		m, _ := cmd.Result()
		reqTotal += parseFloat(m["req"])
		writeTotal += parseFloat(m["write"])
		errTotal += parseFloat(m["err"])
		authFailTotal += parseFloat(m["authfail"])
	}

	distinctIDs, _ := pfCountCmd.Result()
	timestamps, _ := tsCmd.Result()

	f := Features{
		ReqCount:      reqTotal,
		AuthFailCount: authFailTotal,
		DistinctIDs:   float64(distinctIDs),
	}
	if reqTotal > 0 {
		f.WriteRatio = writeTotal / reqTotal
		f.ErrorRatio = errTotal / reqTotal
	}
	f.InterarrivalCV = interarrivalCV(timestamps)
	return f, nil
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// interarrivalCV is the coefficient of variation (stddev/mean) of the gaps
// between the last N request timestamps: bots arriving at regular intervals
// have a low CV, human/organic traffic a higher one.
func interarrivalCV(rawTimestampsMs []string) float64 {
	if len(rawTimestampsMs) < 3 {
		return 0
	}
	ts := make([]float64, 0, len(rawTimestampsMs))
	for _, s := range rawTimestampsMs {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			ts = append(ts, v)
		}
	}
	sort.Float64s(ts)

	gaps := make([]float64, 0, len(ts)-1)
	for i := 1; i < len(ts); i++ {
		gaps = append(gaps, ts[i]-ts[i-1])
	}
	if len(gaps) == 0 {
		return 0
	}

	var mean float64
	for _, g := range gaps {
		mean += g
	}
	mean /= float64(len(gaps))
	if mean == 0 {
		return 0
	}

	var variance float64
	for _, g := range gaps {
		d := g - mean
		variance += d * d
	}
	variance /= float64(len(gaps))

	return math.Sqrt(variance) / mean
}
