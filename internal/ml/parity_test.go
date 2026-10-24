package ml

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type goldenVector struct {
	Features [6]float64 `json:"features"`
	Score    float64    `json:"score"`
}

// TestParity_MatchesScikitLearn is ARCHITECTURE.md §12.5's parity test:
// "Don't ship the model until this test passes." It compares the Go
// scorer against ml/export.py's golden.json, generated from the same
// trained model's own score_samples() output. Skipped (not failed) if the
// Python pipeline hasn't been run yet, since golden.json and the exported
// model are generated artifacts, not checked-in fixtures.
func TestParity_MatchesScikitLearn(t *testing.T) {
	modelPath := filepath.Join("..", "..", "models", "iforest-v1.json")
	goldenPath := filepath.Join("..", "..", "ml", "testdata", "golden.json")

	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("skipping: %s not found (run ml/train.py + ml/export.py first)", modelPath)
	}
	if _, err := os.Stat(goldenPath); err != nil {
		t.Skipf("skipping: %s not found (run ml/export.py first)", goldenPath)
	}

	model, err := LoadModel(modelPath)
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	var golden []goldenVector
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("unmarshal golden file: %v", err)
	}
	if len(golden) == 0 {
		t.Fatal("golden file has no vectors")
	}

	const tolerance = 1e-6
	var maxDiff float64
	for i, gv := range golden {
		x := toFloat32Vector(gv.Features)
		got := model.ScoreSamples(x)
		diff := math.Abs(got - gv.Score)
		if diff > maxDiff {
			maxDiff = diff
		}
		if diff > tolerance {
			t.Errorf("vector %d: got score %v, want %v (diff %v > tolerance %v)", i, got, gv.Score, diff, tolerance)
		}
	}
	t.Logf("compared %d vectors, max diff %v (tolerance %v)", len(golden), maxDiff, tolerance)
}
