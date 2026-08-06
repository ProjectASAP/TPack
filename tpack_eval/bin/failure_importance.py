"""Feature importance analysis for fidelity failure cases using SHAP."""

import argparse
import pathlib
import sys

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd
import shap
import xgboost as xgb
from sklearn.linear_model import LinearRegression
from sklearn.preprocessing import StandardScaler


def parse_card_occur(df: pd.DataFrame) -> pd.DataFrame:
    """Parse card/occur semicolon-separated columns into min/max numeric columns."""
    for col, prefix in [("card", "card"), ("occur", "occur")]:
        mins, maxs = [], []
        for val in df[col]:
            val = str(val)
            if val in ("-", "1", "N/A", "nan"):
                mins.append(np.nan)
                maxs.append(np.nan)
                continue
            parts = val.split(";")
            try:
                nums = [float(x) for x in parts if x not in ("?", "")]
            except ValueError:
                mins.append(np.nan)
                maxs.append(np.nan)
                continue
            if nums:
                mins.append(min(nums))
                maxs.append(max(nums))
            else:
                mins.append(np.nan)
                maxs.append(np.nan)
        df[f"{prefix}_min"] = mins
        df[f"{prefix}_max"] = maxs
    return df


def run_xgb(X, y, feature_cols, stem, query_type, output_dir, suffix=""):
    """Fit XGBoost and produce SHAP bar + beeswarm plots."""
    model = xgb.XGBRegressor(
        n_estimators=200,
        max_depth=4,
        learning_rate=0.1,
        enable_categorical=True,
        random_state=42,
    )
    model.fit(X, y)
    r2 = model.score(X, y)
    print(f"  XGBoost R² = {r2:.3f}")

    explainer = shap.TreeExplainer(model)
    shap_values = explainer(X)

    tag = f"{stem}_{query_type}{suffix}"

    # Bar plot
    out_path = output_dir / f"failure_importance_{tag}.pdf"
    fig, ax = plt.subplots(figsize=(6, 4))
    shap.plots.bar(shap_values, max_display=len(feature_cols), ax=ax, show=False)
    ax.set_title(f"XGBoost feature importance — {query_type} delta ({stem})\nn={len(X)}, R² = {r2:.3f}")
    fig.tight_layout()
    fig.savefig(out_path, dpi=150)
    plt.close(fig)
    print(f"  Wrote {out_path}")

    # Beeswarm plot
    out_path2 = output_dir / f"failure_importance_{tag}_beeswarm.pdf"
    shap.plots.beeswarm(shap_values, max_display=len(feature_cols), plot_size=(8, 5), show=False)
    fig2 = plt.gcf()
    fig2.suptitle(f"XGBoost SHAP beeswarm — {query_type} ({stem})")
    fig2.tight_layout()
    fig2.savefig(out_path2, dpi=150)
    plt.close(fig2)
    print(f"  Wrote {out_path2}")


def run_linear(X, y, feature_cols, stem, query_type, output_dir, suffix=""):
    """Fit linear regression and produce coefficient bar plot."""
    # Drop rows with NaN for linear model (can't handle missing values)
    mask = X.notna().all(axis=1)
    X_clean = X[mask].copy()
    y_clean = y[mask]

    # One-hot encode categoricals
    cat_cols = X_clean.select_dtypes(include="category").columns.tolist()
    if cat_cols:
        X_clean = pd.get_dummies(X_clean, columns=cat_cols, drop_first=True)

    # Standardize for comparable coefficients
    scaler = StandardScaler()
    X_scaled = scaler.fit_transform(X_clean)

    model = LinearRegression()
    model.fit(X_scaled, y_clean)
    r2 = model.score(X_scaled, y_clean)
    print(f"  Linear R² = {r2:.3f}")

    # Coefficient plot (standardized)
    coefs = pd.Series(model.coef_, index=X_clean.columns)
    coefs = coefs.reindex(coefs.abs().sort_values(ascending=True).index)

    tag = f"{stem}_{query_type}{suffix}"
    out_path = output_dir / f"failure_importance_{tag}_linear.pdf"

    fig, ax = plt.subplots(figsize=(6, 4))
    colors = ["#e74c3c" if c > 0 else "#3498db" for c in coefs]
    coefs.plot.barh(ax=ax, color=colors)
    ax.set_xlabel("Standardized coefficient (effect on delta)")
    ax.set_title(f"Linear regression — {query_type} delta ({stem})\nn={len(X_clean)}, R² = {r2:.3f}")
    ax.axvline(0, color="gray", linewidth=0.5)
    fig.tight_layout()
    fig.savefig(out_path, dpi=150)
    plt.close(fig)
    print(f"  Wrote {out_path}")


def main():
    parser = argparse.ArgumentParser(description="Feature importance for failure analysis")
    parser.add_argument("--input", required=True, help="Path to failure_analysis TSV")
    parser.add_argument("--query-type", default="duration",
                        help="Filter to this query_type (default: duration)")
    parser.add_argument("--min-count", type=int, default=0,
                        help="Minimum count to include a row (default: 0, no filter)")
    parser.add_argument("--max-count", type=int, default=0,
                        help="Maximum count to include a row (default: 0, no filter)")
    parser.add_argument("--model", default="both", choices=["xgb", "linear", "both"],
                        help="Model type: xgb, linear, or both (default: both)")
    parser.add_argument("--drop-count", action="store_true",
                        help="Drop count from feature set")
    args = parser.parse_args()

    input_path = pathlib.Path(args.input)
    if not input_path.exists():
        print(f"Error: {input_path} not found", file=sys.stderr)
        sys.exit(1)

    df = pd.read_csv(input_path, sep="\t")
    print(f"Loaded {len(df)} rows from {input_path}")

    # Filter to requested query type
    query_type = args.query_type
    df = df[df["query_type"] == query_type].copy()
    print(f"Filtered to query_type={query_type}: {len(df)} rows")

    # Parse card/occur into min/max
    df = parse_card_occur(df)

    # Coerce count to numeric
    df["count"] = pd.to_numeric(df["count"], errors="coerce")

    # Filter by count range
    if args.min_count > 0:
        df = df[df["count"] >= args.min_count].copy()
        print(f"Filtered to count>={args.min_count}: {len(df)} rows")
    if args.max_count > 0:
        df = df[df["count"] <= args.max_count].copy()
        print(f"Filtered to count<={args.max_count}: {len(df)} rows")

    # Feature columns
    feature_cols = [
        "filter_type", "count",
        "card_min", "card_max", "occur_min", "occur_max", "skew",
        "avg_depth", "shared_edge_rate",
    ]
    # Include percentile as a numeric feature for duration queries
    if "percentile" in df.columns and query_type == "duration":
        df["percentile"] = df["percentile"].str.replace("p", "", regex=False).astype(float)
        feature_cols.insert(0, "percentile")
    if args.drop_count:
        feature_cols.remove("count")

    # Encode categoricals
    for col in ["filter_type"]:
        if col in feature_cols:
            df[col] = df[col].astype("category")

    # Build model dataframe
    model_df = df[feature_cols + ["delta"]].copy()

    # Replace -1 sentinel values with NaN
    for col in ["avg_depth", "shared_edge_rate"]:
        if col in model_df.columns:
            model_df.loc[model_df[col] < 0, col] = np.nan

    # Drop rows where target is NaN
    model_df = model_df.dropna(subset=["delta"])
    print(f"Training on {len(model_df)} rows")

    X = model_df[feature_cols]
    y = model_df["delta"]

    # Build suffix for output filenames
    suffix_parts = []
    if args.min_count > 0:
        suffix_parts.append(f"_mincount{args.min_count}")
    if args.max_count > 0:
        suffix_parts.append(f"_maxcount{args.max_count}")
    if args.drop_count:
        suffix_parts.append("_nocount")
    suffix = "".join(suffix_parts)

    stem = input_path.stem.replace("failure_analysis_", "")
    output_dir = input_path.parent

    if args.model in ("xgb", "both"):
        print("Running XGBoost...")
        run_xgb(X, y, feature_cols, stem, query_type, output_dir, suffix)

    if args.model in ("linear", "both"):
        print("Running Linear Regression...")
        run_linear(X, y, feature_cols, stem, query_type, output_dir, suffix)


if __name__ == "__main__":
    main()
