package ml

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// A single tree: root splits on feature 0 at 0.5. The left leaf sees very
// few samples (isolated quickly -> short path -> anomalous); the right leaf
// sees many (normal -> longer effective path -> less anomalous).
func oneSplitTree() Tree {
	return Tree{
		ChildrenLeft:  []int32{1, -1, -1},
		ChildrenRight: []int32{2, -1, -1},
		Feature:       []int32{0, -1, -1},
		Threshold:     []float64{0.5, 0, 0},
		NNodeSamples:  []int64{4, 1, 3},
	}
}

func TestTree_PathLength(t *testing.T) {
	tree := oneSplitTree()

	short := tree.pathLength([6]float32{0.1, 0, 0, 0, 0, 0}) // -> leaf with n=1
	long := tree.pathLength([6]float32{0.9, 0, 0, 0, 0, 0})  // -> leaf with n=3

	if short >= long {
		t.Fatalf("expected the n=1 leaf to have a shorter path than the n=3 leaf, got short=%v long=%v", short, long)
	}
	// depth=1 both ways; c(1)=0 so short path should be exactly 1.
	if short != 1 {
		t.Errorf("expected path length 1 for the n=1 leaf (depth 1 + c(1)=0), got %v", short)
	}
}

func TestModel_ScoreSamples_ShorterPathIsMoreAnomalous(t *testing.T) {
	m := &Model{
		Trees:      []Tree{oneSplitTree()},
		MaxSamples: 4,
		Threshold:  -10, // irrelevant here; only comparing raw scores
	}

	shortPathScore := m.ScoreSamples([6]float32{0.1, 0, 0, 0, 0, 0})
	longPathScore := m.ScoreSamples([6]float32{0.9, 0, 0, 0, 0, 0})

	// score_samples is "lower = more anomalous", and a shorter path is more
	// anomalous, so its score must be more negative (lower).
	if shortPathScore >= longPathScore {
		t.Fatalf("expected the isolated point's score (%v) to be lower than the normal point's score (%v)", shortPathScore, longPathScore)
	}
}

func TestModel_Flagged(t *testing.T) {
	m := &Model{
		Trees:      []Tree{oneSplitTree()},
		MaxSamples: 4,
		Threshold:  -0.6,
	}
	x := [6]float32{0.1, 0, 0, 0, 0, 0}
	score := m.ScoreSamples(x)
	if got := m.Flagged(x); got != (score < m.Threshold) {
		t.Errorf("Flagged() = %v, want %v (score=%v threshold=%v)", got, score < m.Threshold, score, m.Threshold)
	}
}

func TestAveragePathLengthC(t *testing.T) {
	cases := []struct {
		n    float64
		want float64
	}{
		{0, 0},
		{1, 0},
		{2, 1},
	}
	for _, c := range cases {
		if got := averagePathLengthC(c.n); got != c.want {
			t.Errorf("averagePathLengthC(%v) = %v, want %v", c.n, got, c.want)
		}
	}
	// For n > 2, c(n) should be positive and increase with n.
	c10 := averagePathLengthC(10)
	c100 := averagePathLengthC(100)
	if !(c10 > 0 && c100 > c10) {
		t.Errorf("expected averagePathLengthC to be positive and increasing, got c(10)=%v c(100)=%v", c10, c100)
	}
}

func TestLoadModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	m := Model{
		Trees:        []Tree{oneSplitTree()},
		MaxSamples:   256,
		Offset:       -0.5,
		FeatureNames: []string{"req_count", "write_ratio", "error_ratio", "auth_fail_count", "distinct_ids", "interarrival_cv"},
		Version:      "test-v1",
		Threshold:    -0.6,
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := LoadModel(path)
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if loaded.Version != "test-v1" || len(loaded.Trees) != 1 {
		t.Errorf("loaded model mismatch: %+v", loaded)
	}
}

func TestInterarrivalCV(t *testing.T) {
	// Perfectly regular gaps (bot-like) -> CV should be 0.
	regular := []string{"0", "1000", "2000", "3000"}
	if got := interarrivalCV(regular); math.Abs(got) > 1e-9 {
		t.Errorf("expected CV ~0 for regular intervals, got %v", got)
	}

	// Irregular gaps -> CV should be > 0.
	irregular := []string{"0", "100", "5000", "5200"}
	if got := interarrivalCV(irregular); got <= 0 {
		t.Errorf("expected CV > 0 for irregular intervals, got %v", got)
	}

	// Fewer than 3 timestamps -> not enough gaps to compute a meaningful CV.
	if got := interarrivalCV([]string{"0", "100"}); got != 0 {
		t.Errorf("expected CV 0 for < 3 timestamps, got %v", got)
	}
}
