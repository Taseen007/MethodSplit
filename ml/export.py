"""Export a trained Isolation Forest to the JSON format internal/ml/iforest.go
reads, and write a golden parity file for the Go scorer test
(ARCHITECTURE.md §12.4-§12.5).

Usage:
    python export.py --model models/iforest_raw.joblib --results results.json \
        --version iforest-v1 --out ../models/iforest-v1.json \
        --golden testdata/golden.json
"""
import argparse
import json

import joblib
import numpy as np

from common import FEATURE_NAMES, feature_matrix, load_jsonl


def export_tree(tree, feature_map):
    t = tree.tree_
    # Each tree sees features in its own local subset order (bootstrap
    # feature sampling); feature_map remaps local index -> global index so
    # the exported "feature" array already matches FEATURE_NAMES order and
    # the Go scorer doesn't need to know about per-tree subsets.
    remapped_feature = [
        int(feature_map[f]) if f >= 0 else -1 for f in t.feature
    ]
    return {
        "children_left": t.children_left.tolist(),
        "children_right": t.children_right.tolist(),
        "feature": remapped_feature,
        "threshold": t.threshold.tolist(),
        "n_node_samples": t.n_node_samples.tolist(),
    }


def export_model(model, version, threshold):
    trees = []
    for estimator, feature_subset in zip(model.estimators_, model.estimators_features_):
        trees.append(export_tree(estimator, feature_subset))
    return {
        "trees": trees,
        "max_samples_": float(model.max_samples_),
        "offset_": float(model.offset_),
        "feature_names": FEATURE_NAMES,
        "version": version,
        "threshold": float(threshold),
    }


def build_golden(model, test_rows, n=200, seed=42):
    rng = np.random.default_rng(seed)
    if len(test_rows) > n:
        idx = rng.choice(len(test_rows), size=n, replace=False)
        sample = [test_rows[i] for i in idx]
    else:
        sample = test_rows

    X = feature_matrix(sample)
    scores = model.score_samples(X).tolist()

    return [
        {"features": x, "score": s}
        for x, s in zip(X, scores)
    ]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="models/iforest_raw.joblib")
    parser.add_argument("--results", default="results.json", help="evaluate.py output, for the chosen threshold")
    parser.add_argument("--threshold", type=float, default=None, help="override the threshold from --results")
    parser.add_argument("--version", default="iforest-v1")
    parser.add_argument("--out", default="../models/iforest-v1.json")
    parser.add_argument("--test", default="data/test.jsonl", help="source rows for the golden parity file")
    parser.add_argument("--golden", default="testdata/golden.json")
    parser.add_argument("--golden-n", type=int, default=200)
    args = parser.parse_args()

    model = joblib.load(args.model)

    threshold = args.threshold
    if threshold is None:
        with open(args.results, "r", encoding="utf-8") as f:
            threshold = json.load(f)["model"]["threshold"]

    exported = export_model(model, args.version, threshold)
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(exported, f)
    print(f"exported {len(exported['trees'])} trees, threshold={threshold} -> {args.out}")

    test_rows = load_jsonl(args.test)
    golden = build_golden(model, test_rows, n=args.golden_n)
    with open(args.golden, "w", encoding="utf-8") as f:
        json.dump(golden, f)
    print(f"wrote {len(golden)} golden parity vectors -> {args.golden}")


if __name__ == "__main__":
    main()
