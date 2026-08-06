#!/usr/bin/env python3
"""Plot fidelity score distributions for a given experiment from a report.json."""

import json
import sys

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np


def extract_scores(report, experiment, metric="cosine"):
    """Extract fidelity scores per query type for one experiment.

    metric: "cosine" for cosine_fidelity, "mape" for mape_fidelity.
    """
    fidelity_key = f"{metric}_fidelity"
    results = {}

    for qtype in ["duration_over_time", "rate_over_time", "error_over_time"]:
        qdata = report["reports"].get(qtype, {}).get(experiment, {})
        scores, counts = [], []
        for slice_name, metrics in qdata.items():
            if isinstance(metrics, dict) and fidelity_key in metrics:
                scores.append(metrics[fidelity_key])
                counts.append(metrics.get("count", 0))
        if scores:
            results[qtype] = {"scores": scores, "counts": counts}

    # Graph: per-bucket fidelity (only for cosine mode, graph has its own "fidelity" key)
    gdata = report["reports"].get("graph", {}).get(experiment, {})
    scores = [m["fidelity"] for m in gdata.values() if isinstance(m, dict) and "fidelity" in m]
    if scores:
        results["graph"] = {"scores": scores, "counts": [1] * len(scores)}

    return results


def plot_metric(report, experiment, metric, out_path):
    """Plot fidelity distribution for one metric and save."""
    data = extract_scores(report, experiment, metric=metric)
    if not data:
        print(f"No {metric} data found for {experiment}")
        return

    qtypes = ["duration_over_time", "rate_over_time", "error_over_time", "graph"]
    qtypes = [q for q in qtypes if q in data]

    fig, axes = plt.subplots(1, len(qtypes), figsize=(3.5 * len(qtypes), 3))
    if len(qtypes) == 1:
        axes = [axes]

    bins = np.linspace(0, 100, 21)
    label = metric.upper() + " Fidelity"

    for ax, qtype in zip(axes, qtypes):
        scores = data[qtype]["scores"]
        counts = data[qtype]["counts"]

        ax.hist(scores, bins=bins, edgecolor="white", linewidth=0.5, color="#4878cf")

        # Mark the count-weighted mean
        total = sum(counts)
        if total > 0:
            wmean = sum(s * c for s, c in zip(scores, counts)) / total
            ax.axvline(wmean, color="#e34a33", ls="--", lw=1.5, label=f"wtd mean={wmean:.1f}")
        mean = np.mean(scores)
        ax.axvline(mean, color="#2ca02c", ls=":", lw=1.5, label=f"mean={mean:.1f}")

        xlabel = "Graph Fidelity" if qtype == "graph" else label
        ax.set_xlabel(xlabel)
        ax.set_ylabel("# slices" if qtype != "graph" else "# buckets")
        ax.set_title(qtype.replace("_", " ").title())
        ax.legend(fontsize=7)
        ax.set_xlim(0, 105)

    short_name = experiment.rsplit("_", 1)[0].replace("otel-demo-transformed_", "")
    fig.suptitle(f"{label} Distribution — {short_name}", fontsize=11, y=1.02)
    fig.tight_layout()
    fig.savefig(out_path, bbox_inches="tight")
    print(f"Saved to {out_path}")

    print(f"\n  {metric} per-query summary:")
    for qtype in qtypes:
        s = data[qtype]["scores"]
        c = data[qtype]["counts"]
        total = sum(c)
        wmean = sum(si * ci for si, ci in zip(s, c)) / total if total else 0
        print(f"    {qtype:25s}  n={len(s):3d}  mean={np.mean(s):6.2f}  wtd_mean={wmean:6.2f}  median={np.median(s):6.2f}  min={min(s):6.2f}  <50: {sum(1 for x in s if x < 50)}")


def main():
    report_path = sys.argv[1] if len(sys.argv) > 1 else "output/otel-demo-transformed/202510/1/report.json"
    experiment = sys.argv[2] if len(sys.argv) > 2 else "otel-demo-transformed_tpack_default_1"
    out_base = sys.argv[3] if len(sys.argv) > 3 else "fidelity_dist"

    with open(report_path) as f:
        report = json.load(f)

    print(f"Experiment: {experiment}\n")
    plot_metric(report, experiment, "cosine", f"{out_base}_cosine.pdf")
    print()
    plot_metric(report, experiment, "mape", f"{out_base}_mape.pdf")


if __name__ == "__main__":
    main()
