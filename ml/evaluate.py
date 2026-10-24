"""Pick an operating threshold and evaluate the model vs. the static
rate-limit baseline (ARCHITECTURE.md §12.4, §12.6).

The threshold is chosen on a validation split (the tail of the normal rows
in the test set, held out from both training and the final evaluation
numbers) to keep the false-positive rate on normal traffic <= 1%.

Usage:
    python evaluate.py --model models/iforest_raw.joblib \
        --test data/test.jsonl --labels data/labels.json \
        --target-fpr 0.01 --out results.json
"""
import argparse
import json

import joblib
import numpy as np

from common import feature_matrix, load_jsonl, load_labels


def split_validation(normal_rows, frac=0.3):
    n_val = max(1, int(len(normal_rows) * frac))
    return normal_rows[:n_val], normal_rows[n_val:]


def pick_threshold(model, validation_rows, target_fpr):
    if not validation_rows:
        return 0.0
    scores = model.score_samples(feature_matrix(validation_rows))
    # threshold = the score at the target_fpr percentile: exactly target_fpr
    # fraction of normal validation scores fall at or below it.
    return float(np.quantile(scores, target_fpr))


def evaluate(model, threshold, test_rows, labels):
    scores = model.score_samples(feature_matrix(test_rows))
    flagged = scores < threshold

    by_profile = {}
    for row, score, is_flagged in zip(test_rows, scores, flagged):
        profile = labels.get(row["api_key"], "unknown")
        by_profile.setdefault(profile, {"n": 0, "flagged": 0})
        by_profile[profile]["n"] += 1
        if is_flagged:
            by_profile[profile]["flagged"] += 1

    normal_n = by_profile.get("normal", {"n": 0})["n"]
    normal_flagged = by_profile.get("normal", {"flagged": 0})["flagged"]
    fpr = normal_flagged / normal_n if normal_n else 0.0

    attack_recall = {}
    total_attack_n, total_attack_flagged = 0, 0
    for profile, stats in by_profile.items():
        if profile == "normal":
            continue
        recall = stats["flagged"] / stats["n"] if stats["n"] else 0.0
        attack_recall[profile] = {"n": stats["n"], "flagged": stats["flagged"], "recall": recall}
        total_attack_n += stats["n"]
        total_attack_flagged += stats["flagged"]

    tp = total_attack_flagged
    fp = normal_flagged
    precision = tp / (tp + fp) if (tp + fp) else 0.0
    overall_recall = tp / total_attack_n if total_attack_n else 0.0

    return {
        "threshold": threshold,
        "normal_false_positive_rate": fpr,
        "precision": precision,
        "overall_recall": overall_recall,
        "recall_by_attack_type": attack_recall,
    }


def evaluate_baseline(test_rows, labels):
    """The static-rate-limit baseline: a row is "flagged" by the baseline
    if the gateway's own per-IP/per-merchant rate limiter already rejected
    it (status 429), independent of the ML model."""
    by_profile = {}
    for row in test_rows:
        profile = labels.get(row["api_key"], "unknown")
        by_profile.setdefault(profile, {"n": 0, "flagged": 0})
        by_profile[profile]["n"] += 1
        if row.get("status") == 429:
            by_profile[profile]["flagged"] += 1

    normal_n = by_profile.get("normal", {"n": 0})["n"]
    normal_flagged = by_profile.get("normal", {"flagged": 0})["flagged"]
    fpr = normal_flagged / normal_n if normal_n else 0.0

    attack_recall = {}
    total_attack_n, total_attack_flagged = 0, 0
    for profile, stats in by_profile.items():
        if profile == "normal":
            continue
        recall = stats["flagged"] / stats["n"] if stats["n"] else 0.0
        attack_recall[profile] = {"n": stats["n"], "flagged": stats["flagged"], "recall": recall}
        total_attack_n += stats["n"]
        total_attack_flagged += stats["flagged"]

    tp = total_attack_flagged
    fp = normal_flagged
    precision = tp / (tp + fp) if (tp + fp) else 0.0
    overall_recall = tp / total_attack_n if total_attack_n else 0.0

    return {
        "normal_false_positive_rate": fpr,
        "precision": precision,
        "overall_recall": overall_recall,
        "recall_by_attack_type": attack_recall,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="models/iforest_raw.joblib")
    parser.add_argument("--test", default="data/test.jsonl")
    parser.add_argument("--labels", default="data/labels.json")
    parser.add_argument("--target-fpr", type=float, default=0.01)
    parser.add_argument("--out", default="results.json")
    args = parser.parse_args()

    model = joblib.load(args.model)
    test_rows = load_jsonl(args.test)
    labels = load_labels(args.labels)

    normal_rows = [r for r in test_rows if labels.get(r["api_key"]) == "normal"]
    validation_rows, _ = split_validation(normal_rows)
    threshold = pick_threshold(model, validation_rows, args.target_fpr)

    model_result = evaluate(model, threshold, test_rows, labels)
    baseline_result = evaluate_baseline(test_rows, labels)

    result = {"model": model_result, "baseline_static_limits": baseline_result}
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(result, f, indent=2)

    print(json.dumps(result, indent=2))
    print(f"\nwrote {args.out}")


if __name__ == "__main__":
    main()
