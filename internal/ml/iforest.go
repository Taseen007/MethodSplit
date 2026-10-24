package ml

import (
	"encoding/json"
	"math"
	"os"
)

const eulerGamma = 0.5772156649

// Tree is one Isolation Forest tree, exported from scikit-learn by
// ml/export.py. Feature indices are already remapped to the model's global
// 6-feature order at export time (via estimators_features_[t]), so the Go
// side doesn't need to know about per-tree feature subsets.
type Tree struct {
	ChildrenLeft  []int32   `json:"children_left"`
	ChildrenRight []int32   `json:"children_right"`
	Feature       []int32   `json:"feature"`
	Threshold     []float64 `json:"threshold"`
	NNodeSamples  []int64   `json:"n_node_samples"`
}

// leaf reports whether node is a leaf: scikit-learn's tree_.children_left/
// right use -1 for "no child" on both sides of a leaf.
func (t *Tree) leaf(node int32) bool {
	return t.ChildrenLeft[node] == -1
}

// pathLength walks from the root to a leaf, comparing at float32 precision
// (scikit-learn's internal comparisons are float32; comparing at float64
// would occasionally disagree with the trained thresholds — ARCHITECTURE.md
// §12.4's second export gotcha).
func (t *Tree) pathLength(x [6]float32) float64 {
	node := int32(0)
	depth := 0
	for !t.leaf(node) {
		f := t.Feature[node]
		if x[f] <= float32(t.Threshold[node]) {
			node = t.ChildrenLeft[node]
		} else {
			node = t.ChildrenRight[node]
		}
		depth++
	}
	return float64(depth) + averagePathLengthC(float64(t.NNodeSamples[node]))
}

// averagePathLengthC is c(n): the expected path length of an unsuccessful
// BST search, used to normalize leaf depth by how many samples landed there.
func averagePathLengthC(n float64) float64 {
	switch {
	case n <= 1:
		return 0
	case n == 2:
		return 1
	default:
		return 2*(math.Log(n-1)+eulerGamma) - 2*(n-1)/n
	}
}

// Model is a trained Isolation Forest plus the operating threshold chosen
// by ml/evaluate.py for a target false-positive rate.
type Model struct {
	Trees        []Tree   `json:"trees"`
	MaxSamples   float64  `json:"max_samples_"`
	Offset       float64  `json:"offset_"`
	FeatureNames []string `json:"feature_names"`
	Version      string   `json:"version"`
	Threshold    float64  `json:"threshold"`
}

func LoadModel(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Model
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ScoreSamples mirrors scikit-learn's IsolationForest.score_samples: lower
// (more negative) means more anomalous.
func (m *Model) ScoreSamples(x [6]float32) float64 {
	if len(m.Trees) == 0 {
		return 0
	}
	var sumPath float64
	for i := range m.Trees {
		sumPath += m.Trees[i].pathLength(x)
	}
	meanPath := sumPath / float64(len(m.Trees))
	c := averagePathLengthC(m.MaxSamples)
	if c == 0 {
		return 0
	}
	return -math.Pow(2, -meanPath/c)
}

// Flagged reports whether x scores below the model's chosen threshold.
func (m *Model) Flagged(x [6]float32) bool {
	return m.ScoreSamples(x) < m.Threshold
}
