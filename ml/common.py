"""Shared helpers for train.py / evaluate.py / export.py.

Feature column order must match internal/ml/features.go's FeatureNames —
the Go scorer assumes exported tree "feature" indices refer to this order.
"""
import json

FEATURE_NAMES = [
    "req_count",
    "write_ratio",
    "error_ratio",
    "auth_fail_count",
    "distinct_ids",
    "interarrival_cv",
]


def load_jsonl(path):
    rows = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def load_labels(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def feature_matrix(rows):
    """Rows -> a list of [req_count, write_ratio, ...] in FEATURE_NAMES order."""
    return [[float(r[name]) for name in FEATURE_NAMES] for r in rows]
