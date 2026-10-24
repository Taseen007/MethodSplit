package ml

import "testing"

// BenchmarkScoreSamples measures real scorer latency (ARCHITECTURE.md
// §12.5: "report the real number; don't assume it"), using the same
// hand-built tree as the unit tests when no exported model is present, or
// go test -bench=. against a real model via -args if one has been placed
// at models/iforest-v1.json (loaded through TestParity's path). For a
// quick standalone number, this benchmarks a single tree — see RESULTS.md
// for the measured latency against the real exported model.
func BenchmarkScoreSamples(b *testing.B) {
	m := &Model{
		Trees:      []Tree{oneSplitTree()},
		MaxSamples: 4,
		Threshold:  -0.6,
	}
	x := [6]float32{0.3, 0.1, 0.05, 0, 2, 0.4}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.ScoreSamples(x)
	}
}
