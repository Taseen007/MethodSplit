package ml

import (
	"path/filepath"
	"testing"
)

// BenchmarkScoreSamples_RealModel measures scorer latency against the
// actual exported model (100 trees), not the single-tree toy model —
// this is the number that matters for the gateway's request path.
// Skipped if the model hasn't been exported yet.
func BenchmarkScoreSamples_RealModel(b *testing.B) {
	modelPath := filepath.Join("..", "..", "models", "iforest-v1.json")
	model, err := LoadModel(modelPath)
	if err != nil {
		b.Skipf("skipping: %s not found (run ml/train.py + ml/export.py first): %v", modelPath, err)
	}
	x := [6]float32{5, 0.3, 0.1, 0, 3, 0.4}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = model.ScoreSamples(x)
	}
}
