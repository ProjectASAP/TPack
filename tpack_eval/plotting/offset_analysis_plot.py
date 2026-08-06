"""Visualization for --analyze-offsets distributional evaluation."""

import csv
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
from tpack_eval.plotting.scatter_plot_utils import setup_plot_style


def extract_summary_data(tsv_path: str):
    """Read the _offset_summary.tsv file and return pipeline rows."""
    pipelines = []
    with open(tsv_path) as f:
        reader = csv.reader(f, delimiter="\t")
        for row in reader:
            if not row or not row[0].strip() or row[0] == "pipeline":
                continue
            if len(row) >= 7:
                pipelines.append({
                    "pipeline": row[0],
                    "p50GapMAPE": float(row[1]),
                    "p90GapMAPE": float(row[2]),
                    "p99GapMAPE": float(row[3]),
                    "p50DurMAPE": float(row[4]),
                    "p90DurMAPE": float(row[5]),
                    "p99DurMAPE": float(row[6]),
                })
    return pipelines


def extract_perpair_data(tsv_path: str):
    """Read the per-pair _offset_analysis.tsv file."""
    rows = []
    with open(tsv_path) as f:
        reader = csv.DictReader(f, delimiter="\t")
        for row in reader:
            parsed = {k: row[k] for k in ("parentNodeIdx", "childNodeIdx")}
            parsed["sampleCount"] = int(row["sampleCount"])
            for k, v in row.items():
                if k not in ("parentNodeIdx", "childNodeIdx", "sampleCount"):
                    parsed[k] = float(v)
            rows.append(parsed)
    return rows


def plot_summary_bars(pipelines, output_dir: str):
    """Bar chart comparing ratio vs log pipeline distributional MAPEs."""
    setup_plot_style()

    fig, ax = plt.subplots(1, 1, figsize=(10, 5))

    labels = [p["pipeline"] for p in pipelines]
    x = np.arange(len(labels))
    width = 0.12

    colors_gap = ["#4C72B0", "#6A8FC7", "#A0BFE0"]
    colors_dur = ["#DD8452", "#E8A478", "#F0C8A8"]

    ax.bar(x - 2.5 * width, [p["p50GapMAPE"] * 100 for p in pipelines], width,
           label="p50 Gap", color=colors_gap[0], alpha=0.85)
    ax.bar(x - 1.5 * width, [p["p90GapMAPE"] * 100 for p in pipelines], width,
           label="p90 Gap", color=colors_gap[1], alpha=0.85)
    ax.bar(x - 0.5 * width, [p["p99GapMAPE"] * 100 for p in pipelines], width,
           label="p99 Gap", color=colors_gap[2], alpha=0.85)
    ax.bar(x + 0.5 * width, [p["p50DurMAPE"] * 100 for p in pipelines], width,
           label="p50 Dur", color=colors_dur[0], alpha=0.85)
    ax.bar(x + 1.5 * width, [p["p90DurMAPE"] * 100 for p in pipelines], width,
           label="p90 Dur", color=colors_dur[1], alpha=0.85)
    ax.bar(x + 2.5 * width, [p["p99DurMAPE"] * 100 for p in pipelines], width,
           label="p99 Dur", color=colors_dur[2], alpha=0.85)

    ax.set_ylabel("Distributional MAPE (%)", fontsize=14, fontweight="bold")
    ax.set_title("Distributional Percentile Evaluation: Ratio vs Log", fontsize=14, fontweight="bold")
    ax.set_xticks(x)
    ax.set_xticklabels(labels, fontsize=12)
    ax.legend(fontsize=9, ncol=2)
    ax.grid(axis="y", linestyle="--", alpha=0.5)

    plt.tight_layout()
    out = Path(output_dir) / "offset_distributional_bars.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    plt.savefig(out, dpi=300, bbox_inches="tight", facecolor="white")
    print(f"Saved distributional bar chart to {out}")
    plt.close()


def plot_distributional_comparison(perpair_rows, output_dir: str):
    """Scatter: ratio p99 dur MAPE vs log p99 dur MAPE, colored by sample count."""
    ratio_col = "ratio_p99DurMAPE"
    log_col = "log_p99DurMAPE"

    if ratio_col not in perpair_rows[0] or log_col not in perpair_rows[0]:
        print("Warning: distributional columns not found in TSV, skipping comparison plot")
        return

    setup_plot_style()
    fig, ax = plt.subplots(1, 1, figsize=(8, 7))

    ratio_vals = np.array([r[ratio_col] * 100 for r in perpair_rows])
    log_vals = np.array([r[log_col] * 100 for r in perpair_rows])
    sample_counts = np.array([r["sampleCount"] for r in perpair_rows], dtype=float)

    sc = ax.scatter(ratio_vals, log_vals, c=np.log10(sample_counts + 1),
                    cmap="viridis", alpha=0.6, s=20, edgecolors="none")

    max_val = max(np.percentile(ratio_vals, 98), np.percentile(log_vals, 98))
    ax.plot([0, max_val], [0, max_val], "k--", alpha=0.4, linewidth=1, label="Equal")

    ax.set_xlabel("ratio p99 Duration MAPE (%)", fontsize=12, fontweight="bold")
    ax.set_ylabel("log p99 Duration MAPE (%)", fontsize=12, fontweight="bold")
    ax.set_title("Distributional p99 Duration: Ratio vs Log", fontsize=13, fontweight="bold")

    ax.set_xlim(0, np.percentile(ratio_vals, 98) * 1.1)
    ax.set_ylim(0, np.percentile(log_vals, 98) * 1.1)

    cbar = plt.colorbar(sc, ax=ax)
    cbar.set_label("log10(sample count)", fontsize=10)

    log_worse = np.sum(log_vals > ratio_vals)
    ratio_worse = np.sum(ratio_vals > log_vals)
    ax.text(0.02, 0.98, f"log worse: {log_worse} pairs\nratio worse: {ratio_worse} pairs",
            transform=ax.transAxes, verticalalignment="top", fontsize=10,
            bbox=dict(boxstyle="round,pad=0.3", facecolor="white", alpha=0.8))

    ax.legend(fontsize=10)
    ax.grid(True, linestyle="--", alpha=0.3)

    plt.tight_layout()
    out = Path(output_dir) / "offset_distributional_comparison.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    plt.savefig(out, dpi=300, bbox_inches="tight", facecolor="white")
    print(f"Saved distributional comparison to {out}")
    plt.close()


def generate_all_offset_analysis_plots(input_path: str, output_dir: str = "output/visualizations"):
    """Entry point: read TSV files and generate all offset analysis plots.

    input_path should be the per-pair TSV (*_offset_analysis.tsv).
    The summary TSV (*_offset_summary.tsv) is derived from the same prefix.
    """
    analysis_path = Path(input_path)
    summary_path = analysis_path.parent / analysis_path.name.replace("_offset_analysis.tsv", "_offset_summary.tsv")

    if summary_path.exists():
        pipelines = extract_summary_data(str(summary_path))
        if pipelines:
            plot_summary_bars(pipelines, output_dir)
    else:
        print(f"Warning: summary TSV not found at {summary_path}, skipping summary plots")

    if analysis_path.exists():
        perpair_rows = extract_perpair_data(str(analysis_path))
        if perpair_rows:
            plot_distributional_comparison(perpair_rows, output_dir)
    else:
        print(f"Warning: per-pair TSV not found at {analysis_path}")
