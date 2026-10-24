"""Train an Isolation Forest on normal-only traffic (ARCHITECTURE.md §12.4).

Usage:
    python train.py --train data/train.jsonl --labels data/labels.json \
        --out models/iforest_raw.joblib
"""
import argparse

import joblib
from sklearn.ensemble import IsolationForest

from common import feature_matrix, load_jsonl, load_labels


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--train", default="data/train.jsonl")
    parser.add_argument("--labels", default="data/labels.json")
    parser.add_argument("--out", default="models/iforest_raw.joblib")
    parser.add_argument("--n-estimators", type=int, default=100)
    parser.add_argument("--max-samples", type=int, default=256)
    parser.add_argument("--random-state", type=int, default=42)
    args = parser.parse_args()

    rows = load_jsonl(args.train)
    labels = load_labels(args.labels)

    normal_rows = [r for r in rows if labels.get(r["api_key"]) == "normal"]
    if not normal_rows:
        raise SystemExit(
            f"no normal-labeled rows found in {args.train} "
            f"(labels file: {args.labels}, {len(rows)} total rows read)"
        )

    X = feature_matrix(normal_rows)

    model = IsolationForest(
        n_estimators=args.n_estimators,
        max_samples=min(args.max_samples, len(X)),
        random_state=args.random_state,
    )
    model.fit(X)

    joblib.dump(model, args.out)
    print(f"trained on {len(X)} normal-only rows ({len(rows)} total in {args.train})")
    print(f"n_estimators={args.n_estimators} max_samples={model.max_samples_}")
    print(f"saved model to {args.out}")


if __name__ == "__main__":
    main()
