package ml

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// FeatureLogRecord is one line of logs/features.jsonl: everything needed to
// build a labeled training/test dataset later (ARCHITECTURE.md §12.3), plus
// enough request context to debug a flagged client.
type FeatureLogRecord struct {
	Timestamp      time.Time `json:"timestamp"`
	RequestID      string    `json:"request_id"`
	APIKey         string    `json:"api_key"`
	Route          string    `json:"route"`
	ReqCount       float64   `json:"req_count"`
	WriteRatio     float64   `json:"write_ratio"`
	ErrorRatio     float64   `json:"error_ratio"`
	AuthFailCount  float64   `json:"auth_fail_count"`
	DistinctIDs    float64   `json:"distinct_ids"`
	InterarrivalCV float64   `json:"interarrival_cv"`
	Status         int       `json:"status"`
	Score          float64   `json:"score,omitempty"`
	Flagged        bool      `json:"flagged"`
}

// FeatureLogger appends one JSON line per request to a file. It's used even
// in ml.mode: off, to collect data for later training (ARCHITECTURE.md
// §12.6: "off: features are still logged").
type FeatureLogger struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func NewFeatureLogger(path string) (*FeatureLogger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &FeatureLogger{file: f, enc: json.NewEncoder(f)}, nil
}

func (l *FeatureLogger) Log(rec FeatureLogRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enc.Encode(rec)
}

func (l *FeatureLogger) Close() error {
	return l.file.Close()
}
