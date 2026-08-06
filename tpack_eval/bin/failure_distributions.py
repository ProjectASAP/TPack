"""Worst-N distribution plots for failure analysis TSVs."""

import argparse
import pathlib
import sys

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd


def main():
    parser = argparse.ArgumentParser(
        description="Plot distributions of worst-N queries vs all queries"
    )
    parser.add_argument("--input", required=True, help="Path to failure_analysis TSV")
    parser.add_argument("--query-type", default="duration",
                        help="Filter to this query_type (default: duration)")
    parser.add_argument("--n", type=int, default=2000,
                        help="Number of worst queries to highlight (default: 2000)")
    parser.add_argument("--output", default=None,
                        help="Output PDF path (default: worst{N}_distributions.pdf in input dir)")
    args = parser.parse_args()

    input_path = pathlib.Path(args.input)
    if not input_path.exists():
        print(f"Error: {input_path} not found", file=sys.stderr)
        sys.exit(1)

    df = pd.read_csv(input_path, sep="\t")
    print(f"Loaded {len(df)} rows from {input_path}")

    df = df[df["query_type"] == args.query_type].copy()
    print(f"Filtered to query_type={args.query_type}: {len(df)} rows")

    df["count"] = pd.to_numeric(df["count"], errors="coerce")

    has_percentile = "percentile" in df.columns and df["percentile"].notna().any()
    if has_percentile:
        df["pct"] = df["percentile"].str.replace("p", "", regex=False).astype(int)

    all_df = df.copy()
    worst = df.nsmallest(args.n, "delta")
    n_all = len(all_df)
    n_worst = len(worst)

    print(f"All: {n_all}, Worst {args.n}: {n_worst}")

    n_panels = 4 if has_percentile else 3
    fig, axes = plt.subplots(1, n_panels, figsize=(5 * n_panels, 4.5))

    # 1. avg_depth
    ax = axes[0]
    bins = np.linspace(0, 15, 30)
    ax.hist(all_df["avg_depth"], bins=bins, color="#6baed6", alpha=0.7,
            label=f"All (n={n_all})")
    ax.hist(worst["avg_depth"], bins=bins, color="#d73027", alpha=0.7,
            label=f"Worst {n_worst} (n={n_worst})")
    all_mean = all_df["avg_depth"].mean()
    worst_mean = worst["avg_depth"].mean()
    ax.axvline(all_mean, color="#2171b5", linestyle="--",
               label=f"All mean={all_mean:.1f}")
    ax.axvline(worst_mean, color="#d73027", linestyle="--",
               label=f"Worst {n_worst} mean={worst_mean:.1f}")
    ax.set_xlabel("avg_depth")
    ax.set_ylabel("Number of queries")
    ax.set_title("Distribution of avg_depth")
    ax.legend(fontsize=7)

    # 2. count (log scale)
    ax = axes[1]
    bins = np.logspace(0, 7, 40)
    ax.hist(all_df["count"].clip(lower=1), bins=bins, color="#6baed6", alpha=0.7,
            label=f"All (n={n_all})")
    ax.hist(worst["count"].clip(lower=1), bins=bins, color="#d73027", alpha=0.7,
            label=f"Worst {n_worst} (n={n_worst})")
    all_med = all_df["count"].median()
    worst_med = worst["count"].median()
    ax.axvline(all_med, color="#2171b5", linestyle="--",
               label=f"All median={int(all_med)}")
    ax.axvline(worst_med, color="#d73027", linestyle="--",
               label=f"Worst {n_worst} median={int(worst_med)}")
    ax.set_xscale("log")
    ax.set_xlabel("count (log scale)")
    ax.set_ylabel("Number of queries")
    ax.set_title("Distribution of count")
    ax.legend(fontsize=7)

    # 3. shared_edge_rate
    ax = axes[2]
    bins = np.linspace(0, 1, 30)
    ax.hist(all_df["shared_edge_rate"], bins=bins, color="#6baed6", alpha=0.7,
            label=f"All (n={n_all})")
    ax.hist(worst["shared_edge_rate"], bins=bins, color="#d73027", alpha=0.7,
            label=f"Worst {n_worst} (n={n_worst})")
    all_mean = all_df["shared_edge_rate"].mean()
    worst_mean = worst["shared_edge_rate"].mean()
    ax.axvline(all_mean, color="#2171b5", linestyle="--",
               label=f"All mean={all_mean:.2f}")
    ax.axvline(worst_mean, color="#d73027", linestyle="--",
               label=f"Worst {n_worst} mean={worst_mean:.2f}")
    ax.set_xlabel("shared_edge_rate")
    ax.set_ylabel("Number of queries")
    ax.set_title("Distribution of shared_edge_rate")
    ax.legend(fontsize=7)

    # 4. percentile (if available)
    if has_percentile:
        ax = axes[3]
        pcts = sorted(all_df["pct"].unique())
        all_counts = all_df.groupby("pct").size()
        worst_counts = worst.groupby("pct").size().reindex(pcts, fill_value=0)
        x = np.arange(len(pcts))
        w = 0.35
        ax.bar(x - w / 2, all_counts.values, w, color="#6baed6", alpha=0.7,
               label=f"All (n={n_all})")
        ax.bar(x + w / 2, worst_counts.values, w, color="#d73027", alpha=0.7,
               label=f"Worst {n_worst} (n={n_worst})")
        all_mean = all_df["pct"].mean()
        worst_mean = worst["pct"].mean()
        ax.axvline(all_mean / 10, color="#2171b5", linestyle="--",
                   label=f"All mean={all_mean:.0f}")
        ax.axvline(worst_mean / 10, color="#d73027", linestyle="--",
                   label=f"Worst {n_worst} mean={worst_mean:.0f}")
        ax.set_xticks(x)
        ax.set_xticklabels([f"p{p}" for p in pcts], rotation=45)
        ax.set_xlabel("percentile")
        ax.set_ylabel("Number of queries")
        ax.set_title("Distribution of percentile")
        ax.legend(fontsize=7)

    fig.tight_layout()

    if args.output:
        out_path = pathlib.Path(args.output)
    else:
        stem = input_path.stem.replace("failure_analysis_", "")
        out_path = input_path.parent / f"worst{args.n}_distributions_{stem}.pdf"

    fig.savefig(out_path, dpi=150, bbox_inches="tight")
    plt.close(fig)
    print(f"Wrote {out_path}")


if __name__ == "__main__":
    main()
