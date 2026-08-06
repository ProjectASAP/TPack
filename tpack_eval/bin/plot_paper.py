#!/usr/bin/env python3
"""Unified paper figure generation script.

Usage:
    uv run plot_paper --mode cost_fidelity --report output/.../report.json
    uv run plot_paper --mode query_fidelity --report output/.../report.json --rca-report output/.../report.json
"""

import argparse
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
import orjson
from matplotlib import ticker


plt.rcParams.update(
    {
        "font.size": 10,
        "axes.labelsize": 10,
        "axes.titlesize": 11,
        "xtick.labelsize": 9,
        "ytick.labelsize": 9,
        "lines.linewidth": 1.5,
        "axes.grid": True,
        "grid.alpha": 0.3,
        "grid.linewidth": 0.5,
    }
)

# Larger-font rc overrides for fig7 (cost-fidelity), fig9 (query-fidelity),
# fig10 (rca). Same figsize; just bigger labels/ticks/legend and bolder grid.
_BIG_RC = {
    "axes.labelsize": 13,
    "axes.titlesize": 13,
    "xtick.labelsize": 11,
    "ytick.labelsize": 11,
    "legend.fontsize": 11,
    "grid.linewidth": 1.0,
    "grid.alpha": 0.45,
}

from tpack_eval.plotting.data import CostConfig, ReportParser


# ── Unified visual styles across all figures ──
# High-contrast conference palette — TPack pops, baselines recede
APPROACH_STYLES = {
    "TPack": {"marker": "*", "color": "#d62728", "size": 140, "zorder": 8},  # red
    "TPack-Template": {
        "marker": "X",
        "color": "#ff7f0e",
        "size": 120,
        "zorder": 7,
    },  # orange
    "Head": {"marker": "D", "color": "#555555", "size": 80, "zorder": 5},  # medium gray
    "Tail": {
        "marker": "s",
        "color": "#1f77b4",
        "size": 100,
        "zorder": 5,
    },  # standard blue
    "Hindsight": {"marker": "^", "color": "#2ca02c", "size": 100, "zorder": 5},  # green
    "Sifter": {"marker": "P", "color": "#9467bd", "size": 100, "zorder": 5},  # purple
}
ERRORBAR_STYLE = {"capsize": 3, "capthick": 0.8, "linewidth": 0.8}

# Display names in plots — mirrors \sysname in the paper. Internal keys stay
# "TPack" to avoid cascading changes through report parsing.
SYS_NAME = "T-Pack"
DISPLAY_NAMES = {
    "TPack": SYS_NAME,
    "TPack-Template": f"{SYS_NAME}-Template",
    "Hindsight": "Hindsight / Tail-Edge",
    "Tail": "Tail-Backend",
}


def display_name(key: str) -> str:
    return DISPLAY_NAMES.get(key, key)


# Per-metric colors (for bar charts and multi-metric plots)
METRIC_COLORS = {
    "Rate": "#1f77b4",  # blue
    "Error": "#d62728",  # red
    "Duration": "#ff7f0e",  # orange
    "Graph": "#2ca02c",  # green
}
# ── Helpers for fig8/fig9 ──

_report_cache = {}  # path → (report_data, experiments)


def load_report_data(report_path):
    """Load report.json and parse experiments (single file read, cached)."""
    key = str(report_path)
    if key in _report_cache:
        return _report_cache[key]
    with open(report_path, "rb") as f:
        report_data = orjson.loads(f.read())
    parser = ReportParser(CostConfig())
    experiments = parser.parse_report_data(report_data)
    _report_cache[key] = (report_data, experiments)
    return report_data, experiments


def load_and_merge_reports(report_paths):
    """Load multiple report files and deep-merge their data."""
    merged_data = {}
    merged_experiments = []
    for p in report_paths:
        data, exps = load_report_data(p)
        # Deep merge: reports → metric → compressor entries
        if "reports" in data and "reports" in merged_data:
            for metric, compressors in data["reports"].items():
                if metric not in merged_data["reports"]:
                    merged_data["reports"][metric] = compressors
                elif isinstance(compressors, dict):
                    merged_data["reports"][metric].update(compressors)
        else:
            merged_data.update(data)
        merged_experiments.extend(exps)
    return merged_data, merged_experiments


def _first_report(report):
    """Extract the first report path from a list or single path."""
    if report is None:
        return None
    if isinstance(report, list):
        return report[0] if report else None
    return report


MIN_COUNT = 1  # Minimum span count to include a query group


def _aggregate_fidelity(data, min_count, weighted, fidelity_key="mape_fidelity"):
    """Compute mean fidelity, filtering groups with < min_count spans.
    If weighted=True, use span-count-weighted average; otherwise simple mean."""
    fids, counts = [], []
    for d in data.values():
        if not isinstance(d, dict) or fidelity_key not in d:
            continue
        count = d.get("count", 0)
        if count < min_count:
            continue
        fids.append(d[fidelity_key])
        counts.append(count)
    if not fids:
        return None
    if weighted:
        return float(np.average(fids, weights=counts))
    return float(np.mean(fids))


def extract_fidelity_per_compressor(
    report_data, experiments, weighted=True, min_count=None
):
    """Extract per-query fidelity for each compressor.
    If weighted=True, use span-count-weighted mean; otherwise simple mean.
    Filters groups with >= min_count baseline spans (no threshold for error).
    Returns dict of compressor_key → {rate, error, dur, graph, cost}."""
    if min_count is None:
        min_count = MIN_COUNT
    cost_lookup = {exp.compressor_key: exp.total_cost for exp in experiments}

    results = {}
    for exp in experiments:
        key = exp.compressor_key

        rate_data = report_data["reports"].get("rate_over_time", {}).get(key, {})
        rate = _aggregate_fidelity(rate_data, min_count, weighted)

        error_data = report_data["reports"].get("error_over_time", {}).get(key, {})
        error = _aggregate_fidelity(error_data, 0, weighted)

        dur_fids, dur_counts = [], []
        for pct in [
            "duration_over_time_p50",
            "duration_over_time_p90",
            "duration_over_time_p99",
            "duration_over_time",
        ]:
            dur_data = report_data["reports"].get(pct, {}).get(key, {})
            for d in dur_data.values():
                if isinstance(d, dict) and "mape_fidelity" in d:
                    count = d.get("count", 0)
                    if count < min_count:
                        continue
                    dur_fids.append(d["mape_fidelity"])
                    dur_counts.append(count)
        if dur_fids:
            dur = (
                float(np.average(dur_fids, weights=dur_counts))
                if weighted
                else float(np.mean(dur_fids))
            )
        else:
            dur = None

        graph_data = report_data["reports"].get("graph", {}).get(key, {})
        graph_fids = [
            v["fidelity"]
            for v in graph_data.values()
            if isinstance(v, dict) and "fidelity" in v
        ]
        graph = np.mean(graph_fids) if graph_fids else None

        graph_bin_data = report_data["reports"].get("graph_binary", {}).get(key, {})
        graph_bin_fids = [
            v["fidelity"]
            for v in graph_bin_data.values()
            if isinstance(v, dict) and "fidelity" in v
        ]
        graph_binary = np.mean(graph_bin_fids) if graph_bin_fids else None

        results[key] = {
            "rate": rate,
            "error": error,
            "dur": dur,
            "graph": graph,
            "graph_binary": graph_binary,
            "cost": cost_lookup.get(key, 0),
            "is_head": exp.is_head_sampling,
            "name": exp.name,
            "experiment_name": exp.experiment_name,
        }
    return results


HEAD_RATES_TO_SHOW = {1, 2, 3, 5, 10, 20, 50, 100}


def classify_experiment(exp_data):
    """Classify experiment into (approach, label).
    approach: key into APPROACH_STYLES ("TPack", "Head", "Tail", "Hindsight", "Sifter")
    label: display label for annotation (e.g., "1:100" for head, "TPack" for tpack)
    Returns None to skip (e.g., ablation variants, unwanted head rates)."""
    ename = exp_data["experiment_name"]
    if exp_data["is_head"]:
        try:
            rate = int(ename.split("_")[0] if "_" in ename else ename)
        except ValueError:
            return None
        if rate not in HEAD_RATES_TO_SHOW:
            return None
        return "Head", f"1:{rate}"
    if ename in ("tpack", "default"):
        return "TPack", "TPack"
    if ename == "template":
        return "TPack-Template", "TPack-Template"
    if ename == "tail":
        return "Tail", "Tail"
    if ename == "hindsight":
        return "Hindsight", "Hindsight"
    if ename.startswith("sifter"):
        return "Sifter", "Sifter"
    return None  # skip ablation variants, strawman, etc.


def plot_scatter(
    ax,
    points,
    title,
    xlabel,
    ylabel="Cost ($)",
    show_title=True,
    annotate_heads=True,
    head_label_pos="top-left",
):
    """Plot a scatter with all approaches.
    points: list of (approach, label, x, y) tuples.
    approach is a key into APPROACH_STYLES.

    Tail, Hindsight, Sifter are rendered as horizontal dashed cost-reference
    lines (axhline) rather than fidelity/cost scatter points — their fidelity
    numbers are biased-sampling artifacts and not directly comparable.

    head_label_pos: "top-left" (default) or "bottom-right" — anchor direction
    for the 1:k rate labels next to each head-sampling marker."""
    if head_label_pos == "top-left":
        _head_xytext, _head_ha, _head_va = (-6, 4), "right", "bottom"
    elif head_label_pos == "bottom-right":
        _head_xytext, _head_ha, _head_va = (6, -4), "left", "top"
    elif head_label_pos == "right":
        _head_xytext, _head_ha, _head_va = (6, 0), "left", "center"
    else:
        raise ValueError(f"unknown head_label_pos: {head_label_pos}")
    # Group points by (approach, label)
    groups = {}
    for approach, label, x, y in points:
        groups.setdefault((approach, label), []).append((x, y))

    legend_added = set()

    # Plot head sampling groups first (sorted by rate)
    head_items = [(k, v) for k, v in groups.items() if k[0] == "Head"]
    head_items.sort(
        key=lambda item: int(item[0][1].split(":")[1]) if ":" in item[0][1] else 0
    )
    hs = APPROACH_STYLES["Head"]
    for (approach, label), vals in head_items:
        xs, ys = zip(*vals)
        mx, my = np.mean(xs), np.mean(ys)
        ax.errorbar(
            mx,
            my,
            xerr=np.std(xs),
            yerr=np.std(ys),
            fmt="none",
            color=hs["color"],
            zorder=hs["zorder"],
            **ERRORBAR_STYLE,
        )
        ax.scatter(
            mx,
            my,
            marker=hs["marker"],
            s=hs["size"],
            color=hs["color"],
            zorder=hs["zorder"],
            edgecolors="black",
            linewidths=0.5,
            label="Head" if "Head" not in legend_added else None,
        )
        legend_added.add("Head")
        if annotate_heads:
            ax.annotate(
                label,
                (mx, my),
                xytext=_head_xytext,
                textcoords="offset points",
                color=hs["color"],
                fontsize=plt.rcParams["xtick.labelsize"],
                ha=_head_ha,
                va=_head_va,
                zorder=hs["zorder"] + 1,
            )

    # TPack / TPack-Template: scatter at (fidelity, cost) with errorbars (unchanged)
    for approach_name in ["TPack", "TPack-Template"]:
        style = APPROACH_STYLES[approach_name]
        approach_vals = []
        for (approach, label), vals in groups.items():
            if approach == approach_name:
                approach_vals.extend(vals)
        if not approach_vals:
            continue
        xs, ys = zip(*approach_vals)
        mx, my = np.mean(xs), np.mean(ys)
        ax.errorbar(
            mx,
            my,
            xerr=np.std(xs),
            yerr=np.std(ys),
            fmt="none",
            color=style["color"],
            zorder=style["zorder"],
            **ERRORBAR_STYLE,
        )
        ax.scatter(
            mx,
            my,
            marker=style["marker"],
            s=style["size"],
            color=style["color"],
            zorder=style["zorder"],
            edgecolors="black",
            linewidths=0.5,
            label=display_name(approach_name),
        )

    # Tail / Hindsight / Sifter: horizontal cost dashlines across the full axis.
    # No inline labels — the shared legend already identifies each line.
    for approach_name in ["Tail", "Hindsight", "Sifter"]:
        style = APPROACH_STYLES[approach_name]
        approach_vals = []
        for (approach, label), vals in groups.items():
            if approach == approach_name:
                approach_vals.extend(vals)
        if not approach_vals:
            continue
        _, ys = zip(*approach_vals)
        my = np.mean(ys)
        ax.axhline(
            my,
            color=style["color"],
            linestyle="--",
            linewidth=1.6,
            alpha=0.85,
            zorder=style["zorder"] - 2,
            label=display_name(approach_name),
        )

    ax.set_xlabel(xlabel)
    ax.set_ylabel(ylabel)
    if show_title and title:
        ax.set_title(title, fontweight="bold")
    ax.set_yscale("log")
    ax.set_xlim(0, 105)


def add_shared_legend(fig, axes, fontsize=8, nrows=2, loc="top"):
    """Collect handles/labels from all subplots, dedupe, render in nrows rows.
    loc="top" puts the legend above the grid; loc="bottom" below."""
    flat = (
        axes.flat
        if hasattr(axes, "flat")
        else (axes if isinstance(axes, (list, tuple)) else [axes])
    )
    seen, dedup_h, dedup_l = set(), [], []
    for ax in flat:
        for h, l in zip(*ax.get_legend_handles_labels()):
            if l not in seen:
                seen.add(l)
                dedup_h.append(h)
                dedup_l.append(l)
    if not dedup_h:
        return
    ncol = max(1, (len(dedup_h) + nrows - 1) // nrows)
    if loc == "top":
        # Reserve top strip in absolute inches so layout is consistent across
        # figures of different heights. tight_layout(pad=0.3) keeps matplotlib's
        # own inner padding small so the rendered gap matches the requested one.
        fig_h = fig.get_size_inches()[1]
        legend_row_h = 0.25
        gap_h = 0.04
        reserve_in = legend_row_h * nrows + gap_h
        top_rect = max(0.70, 1.0 - reserve_in / fig_h)
        fig.tight_layout(rect=[0, 0, 1, top_rect], pad=0.3)
        anchor_y = top_rect + gap_h / fig_h
        fig.legend(
            dedup_h,
            dedup_l,
            loc="lower center",
            bbox_to_anchor=(0.5, anchor_y),
            ncol=ncol,
            frameon=False,
            fontsize=fontsize,
        )
    else:
        bot_rect = 0.12 if nrows >= 2 else 0.06
        fig.tight_layout(rect=[0, bot_rect, 1, 1])
        fig.legend(
            dedup_h,
            dedup_l,
            loc="lower center",
            bbox_to_anchor=(0.5, 0.0),
            ncol=ncol,
            frameon=False,
            fontsize=fontsize,
        )


# ── Figure modes ──


def collect_duration_fidelity(report_data, compressor_key):
    """Mean duration fidelity across p50/p90/p99/overall for one compressor."""
    fids = []
    for pct in (
        "duration_over_time_p50",
        "duration_over_time_p90",
        "duration_over_time_p99",
        "duration_over_time",
    ):
        d = report_data["reports"].get(pct, {}).get(compressor_key, {})
        fids.extend(
            [
                v["mape_fidelity"]
                for v in d.values()
                if isinstance(v, dict) and "mape_fidelity" in v
            ]
        )
    if not fids:
        return None
    return float(np.mean(fids))


def collect_mean_fidelity(report_data, compressor_key):
    """Compute mean fidelity: average per metric, then average across metrics.
    Metrics: rate, error, duration, graph. Each contributes equally."""
    metric_means = []

    # Rate
    rate_data = report_data["reports"].get("rate_over_time", {}).get(compressor_key, {})
    rate_fids = [
        d["mape_fidelity"]
        for d in rate_data.values()
        if isinstance(d, dict) and "mape_fidelity" in d
    ]
    if rate_fids:
        metric_means.append(np.mean(rate_fids))

    # Error
    error_data = (
        report_data["reports"].get("error_over_time", {}).get(compressor_key, {})
    )
    error_fids = [
        d["mape_fidelity"]
        for d in error_data.values()
        if isinstance(d, dict) and "mape_fidelity" in d
    ]
    if error_fids:
        metric_means.append(np.mean(error_fids))

    # Duration (average across p50/p90/p99/overall)
    dur_fids = []
    for pct in [
        "duration_over_time_p50",
        "duration_over_time_p90",
        "duration_over_time_p99",
        "duration_over_time",
    ]:
        data = report_data["reports"].get(pct, {}).get(compressor_key, {})
        dur_fids.extend(
            [
                d["mape_fidelity"]
                for d in data.values()
                if isinstance(d, dict) and "mape_fidelity" in d
            ]
        )
    if dur_fids:
        metric_means.append(np.mean(dur_fids))

    # Graph (edge frequency)
    graph_data = report_data["reports"].get("graph", {}).get(compressor_key, {})
    graph_fids = [
        d["fidelity"]
        for d in graph_data.values()
        if isinstance(d, dict) and "fidelity" in d
    ]
    if graph_fids:
        metric_means.append(np.mean(graph_fids))

    if not metric_means:
        return None
    return float(np.mean(metric_means))


def _collect_fig8_points(report_paths):
    """Collect (approach, label, fidelity, cost) points from one or more report files.
    For multi-report (e.g., RE2 with multiple services), averages fidelity per
    compressor across reports, then classifies."""
    if not report_paths:
        return []

    # For single report, straightforward
    if len(report_paths) == 1:
        report_data, experiments = load_report_data(report_paths[0])
        points = []
        for exp in experiments:
            fid = collect_mean_fidelity(report_data, exp.compressor_key)
            if fid is None:
                continue
            dummy = {
                "is_head": exp.is_head_sampling,
                "experiment_name": exp.experiment_name,
            }
            cls = classify_experiment(dummy)
            if cls is None:
                continue
            approach, label = cls
            points.append((approach, label, fid, exp.total_cost))
        return points

    # Multi-report: aggregate by (experiment_name, iteration) across reports
    from collections import defaultdict

    agg = defaultdict(lambda: {"fids": [], "costs": []})
    for rp in report_paths:
        report_data, experiments = load_report_data(rp)
        for exp in experiments:
            fid = collect_mean_fidelity(report_data, exp.compressor_key)
            if fid is None:
                continue
            key = (exp.experiment_name, exp.iteration, exp.is_head_sampling)
            agg[key]["fids"].append(fid)
            agg[key]["costs"].append(exp.total_cost)

    points = []
    for (ename, iteration, is_head), data in agg.items():
        avg_fid = np.mean(data["fids"])
        avg_cost = np.mean(data["costs"])
        dummy = {"is_head": is_head, "experiment_name": ename}
        cls = classify_experiment(dummy)
        if cls is None:
            continue
        approach, label = cls
        points.append((approach, label, avg_fid, avg_cost))
    return points


def plot_fig8_cost_fidelity(
    data_dir: Path,
    output_dir: Path,
    report: Path = None,
    datasets: list = None,
    **kwargs,
):
    """Fig 8: Cost vs. mean fidelity scatter plot with 4 dataset subplots.
    --datasets "Label:path1.json,path2.json" for each subplot."""
    if not datasets:
        # Fallback: single report mode
        if report is None:
            print("ERROR: --report or --datasets required for cost_fidelity")
            return
        datasets = [f":{report}"]

    # Parse datasets: "Label:path1,path2,..." or glob patterns
    import glob as glob_mod

    dataset_configs = []
    for ds in datasets:
        if ":" in ds:
            label, paths_str = ds.split(":", 1)
        else:
            label, paths_str = "", ds
        # Expand globs
        report_paths = []
        for p in paths_str.split(","):
            expanded = sorted(glob_mod.glob(p.strip()))
            report_paths.extend([Path(ep) for ep in expanded])
        if not report_paths:
            print(f"Warning: no reports found for '{label}': {paths_str}")
            continue
        dataset_configs.append((label, report_paths))

    n = len(dataset_configs)
    if n == 0:
        print("ERROR: no datasets to plot")
        return

    with plt.rc_context(_BIG_RC):
        ncols = min(n, 2)
        nrows = (n + ncols - 1) // ncols
        fig, axes = plt.subplots(nrows, ncols, figsize=(3.5 * ncols, 2.8 * nrows))
        if n == 1:
            axes = [axes]
        else:
            axes = axes.flatten()

        for i, (label, report_paths) in enumerate(dataset_configs):
            points = _collect_fig8_points(report_paths)
            # Drop TPack-Template on fig8: it's a graph-ablation artifact (fig13),
            # not part of the cost-fidelity story across datasets.
            points = [p for p in points if p[0] != "TPack-Template"]
            plot_scatter(
                axes[i],
                points,
                label,
                "Mean Fidelity (%)",
                show_title=True,
                annotate_heads=True,
            )

        # Remove redundant labels: x-label only on bottom row, y-label only on left column
        for i in range(n):
            row, col = divmod(i, ncols)
            if row < nrows - 1:
                axes[i].set_xlabel("")
            if col > 0:
                axes[i].set_ylabel("")

        # Hide unused subplots
        for j in range(n, len(axes)):
            axes[j].set_visible(False)

        add_shared_legend(fig, axes, fontsize=_BIG_RC["legend.fontsize"])
        out_path = output_dir / "cost_fidelity.pdf"
        plt.savefig(out_path, bbox_inches="tight")
        print(f"Saved {out_path}")


def plot_fig9_query_fidelity(
    data_dir: Path, output_dir: Path, report=None, rca_report: Path = None, **kwargs
):
    """Fig 9: Per-query-type fidelity vs cost (5 subplots) with all approaches."""
    report = _first_report(report)
    if report is None:
        print("ERROR: --report required for query_fidelity")
        return

    report_data, experiments = load_report_data(report)
    per_comp = extract_fidelity_per_compressor(
        report_data, experiments, weighted=False, min_count=1
    )

    # Build per-query points using classify_experiment
    query_points = {"Rate": [], "Error": [], "Duration": [], "Graph": []}
    for key, d in per_comp.items():
        cls = classify_experiment(d)
        if cls is None:
            continue
        approach, label = cls
        if d["rate"] is not None:
            query_points["Rate"].append((approach, label, d["rate"], d["cost"]))
        if d["error"] is not None:
            query_points["Error"].append((approach, label, d["error"], d["cost"]))
        if d["dur"] is not None:
            query_points["Duration"].append((approach, label, d["dur"], d["cost"]))
        if d["graph"] is not None:
            query_points["Graph"].append((approach, label, d["graph"], d["cost"]))
    # Remove empty query types
    query_points = {k: v for k, v in query_points.items() if v}

    with plt.rc_context(_BIG_RC):
        n_plots = len(query_points)
        ncols = 2
        nrows = (n_plots + ncols - 1) // ncols
        fig, axes = plt.subplots(nrows, ncols, figsize=(3.5 * ncols, 2.8 * nrows))
        if n_plots == 1:
            axes = [axes]
        else:
            axes = axes.flatten()

        for i, (query_name, pts) in enumerate(query_points.items()):
            xlabel = f"{query_name} Fidelity (%)"
            pos = "top-left" if query_name == "Graph" else "bottom-right"
            plot_scatter(
                axes[i], pts, query_name, xlabel, show_title=True, head_label_pos=pos
            )

        # Remove redundant y-labels: only left column
        for i in range(n_plots):
            row, col = divmod(i, ncols)
            if col > 0:
                axes[i].set_ylabel("")

        # Hide unused subplots
        for j in range(n_plots, len(axes)):
            axes[j].set_visible(False)

        add_shared_legend(fig, axes, fontsize=_BIG_RC["legend.fontsize"])
        out_path = output_dir / "query_fidelity.pdf"
        plt.savefig(out_path, bbox_inches="tight")
        print(f"Saved {out_path}")


def plot_fig11_scalability(data_dir: Path, output_dir: Path, **kwargs):
    """Fig 11: Unit cost (cost per billion spans) vs spans per minute on Uber."""
    data_path = data_dir / "fig11_scalability.json"
    if not data_path.exists():
        print(f"ERROR: {data_path} not found")
        return

    with open(data_path, "rb") as f:
        data = orjson.loads(f.read())

    EGRESS_PER_GB = 0.10  # $/GB egress (matches CostConfig)
    COMPUTE_PER_HR = 0.16  # $/hr CPU (matches CostConfig.cpu_per_hour)
    COMPUTE_PER_S = COMPUTE_PER_HR / 3600

    spans_per_min = [
        d["spans_millions"] for d in data
    ]  # single 60s bucket = per minute

    # Cost per billion spans for each approach
    tpack_unit = []
    head50_unit = []
    head100_unit = []

    for d in data:
        s = d["spans_millions"] * 1e6  # total spans

        # TPack: model transmission + compute
        tx = d["model_gz_bytes"] / 1e9 * EGRESS_PER_GB
        compute = d["total_seconds"] * COMPUTE_PER_S
        tpack_unit.append((tx + compute) / s * 1e9)

        # Head: raw gzip transmission + compute, divided by sampling rate
        raw_tx = d["raw_gz_bytes"] / 1e9 * EGRESS_PER_GB
        raw_comp = (
            d.get("raw_gzip_compress_seconds", 0)
            + d.get("raw_gzip_decompress_seconds", 0)
        ) * COMPUTE_PER_S
        raw_total = raw_tx + raw_comp
        head50_unit.append((raw_total / 50) / s * 1e9)
        head100_unit.append((raw_total / 100) / s * 1e9)

    fig, ax = plt.subplots(figsize=(4.5, 3.2))

    hs = APPROACH_STYLES["Head"]
    gs = APPROACH_STYLES["TPack"]

    # Full-weight lines (they carry the scaling signal here, unlike the
    # connector lines in ablation plots) + scatter markers in the same
    # style/size APPROACH_STYLES uses elsewhere.
    def _draw_series(xs, ys, style, label, linestyle="-", alpha=1.0):
        ax.plot(
            xs,
            ys,
            linestyle=linestyle,
            color=style["color"],
            alpha=alpha,
            zorder=style["zorder"] - 1,
            linewidth=1.5,
        )
        ax.scatter(
            xs,
            ys,
            marker=style["marker"],
            s=style["size"],
            color=style["color"],
            zorder=style["zorder"],
            edgecolors="black",
            linewidths=0.5,
            alpha=alpha,
            label=label,
        )

    _draw_series(spans_per_min, head50_unit, hs, "Head 1:50", linestyle="-", alpha=0.7)
    _draw_series(spans_per_min, head100_unit, hs, "Head 1:100", linestyle="--")
    _draw_series(spans_per_min, tpack_unit, gs, SYS_NAME, linestyle="-")

    ax.set_xlabel("Spans per minute (millions)")
    ax.set_ylabel("Cost per billion spans ($)")
    ax.legend(loc="upper right", fontsize=8)

    plt.tight_layout()
    out_path = output_dir / "scalability.pdf"
    plt.savefig(out_path, bbox_inches="tight")
    print(f"Saved {out_path}")


def plot_fig3_tradeoff(data_dir: Path, output_dir: Path, report=None, **kwargs):
    """Fig 3: Cost-fidelity tradeoff overview (§2 motivation).
    Single scatter: cost vs mean fidelity. Each TPack config is its own point."""
    if report is None:
        print("ERROR: --report required for tradeoff")
        return

    # Merge multiple report files if provided
    if isinstance(report, list):
        report_data, experiments = load_and_merge_reports(report)
    else:
        report_data, experiments = load_report_data(report)

    head_rates_to_show = {"1:1", "1:2", "1:3", "1:5", "1:10", "1:20", "1:50", "1:100"}

    # Group experiments by approach label
    # TPack feat configs get per-config labels so each config has its own mean±std across seeds
    approach_points = {}  # label → [(fidelity, cost), ...]
    for exp in experiments:
        fid = collect_mean_fidelity(report_data, exp.compressor_key)
        if fid is None:
            continue
        ename = exp.experiment_name
        cost = exp.total_cost

        if ename in ("tpack", "default"):
            # Only show the default TPack config as T-Pack on Fig 3.
            label = "TPack-default"
        elif ename.startswith("feat"):
            continue  # feature-ablation variants live in Fig 8 tradeoff scatter
        elif ename == "tail":
            label = "Tail"
        elif ename == "hindsight":
            label = "Hindsight"
        elif ename.startswith("sifter"):
            label = "Sifter"
        elif exp.is_head_sampling:
            rate = ename.split("_")[0] if "_" in ename else ename
            label = f"1:{rate}"
            if label not in head_rates_to_show:
                continue
        else:
            continue

        approach_points.setdefault(label, []).append((fid, cost))

    gs = APPROACH_STYLES["TPack"]
    hs = APPROACH_STYLES["Head"]

    fig, ax = plt.subplots(figsize=(3.5, 2.625))
    ax.set_xlim(0, 105)

    tpack_means = []  # [(mx, my)] for TPack variants
    # Render dashline approaches first (so markers draw over them)
    for label, vals in approach_points.items():
        if label not in ("Tail", "Hindsight", "Sifter"):
            continue
        style = APPROACH_STYLES[label]
        _, ys = zip(*vals)
        my = np.mean(ys)
        ax.axhline(
            my,
            color=style["color"],
            linestyle="--",
            linewidth=1.6,
            alpha=0.85,
            zorder=style["zorder"] - 2,
        )
        # Inline right-edge label placed UNDER the dashline (Fig 3 has no legend).
        # Small offset so the label sits visibly below the line, not touching it.
        ax.annotate(
            display_name(label),
            (104, my),
            xytext=(0, -5),
            textcoords="offset points",
            color=style["color"],
            fontsize=plt.rcParams["xtick.labelsize"] - 1,
            va="top",
            ha="right",
            alpha=0.9,
            zorder=style["zorder"] - 1,
        )

    # Render Head and TPack as scatter markers
    for label, vals in approach_points.items():
        xs, ys = zip(*vals)
        mx, my = np.mean(xs), np.mean(ys)
        sx, sy = np.std(xs), np.std(ys)

        if label.startswith("1:"):
            style = hs
        elif label.startswith("TPack-"):
            style = gs
            tpack_means.append((mx, my))
        else:
            continue  # Tail/Hindsight/Sifter already rendered as dashlines

        ax.errorbar(
            mx,
            my,
            xerr=sx,
            yerr=sy,
            fmt="none",
            color=style["color"],
            zorder=style["zorder"],
            **ERRORBAR_STYLE,
        )
        ax.scatter(
            mx,
            my,
            marker=style["marker"],
            s=style["size"],
            color=style["color"],
            zorder=style["zorder"],
            edgecolors="black",
            linewidths=0.5,
        )

        # Head rate labels: fixed top-left offset (consistent with figs 7/9/10).
        if label.startswith("1:"):
            ax.annotate(
                label,
                (mx, my),
                xytext=(-6, 4),
                textcoords="offset points",
                color=style["color"],
                fontsize=plt.rcParams["xtick.labelsize"],
                ha="right",
                va="bottom",
                zorder=style["zorder"] + 1,
            )

    # Label highest-fidelity TPack variant with the system display name.
    if tpack_means:
        best = max(tpack_means, key=lambda p: p[0])
        ax.annotate(
            SYS_NAME,
            best,
            xytext=(8, 5),
            textcoords="offset points",
            fontweight="bold",
            color=gs["color"],
            arrowprops=dict(arrowstyle="->", color=gs["color"], alpha=0.5, lw=0.8),
        )

    ax.set_xlabel("Mean Fidelity (%)")
    ax.set_ylabel("Cost ($)")
    ax.set_yscale("log")
    ax.set_xlim(0, 105)

    plt.tight_layout()
    out_path = output_dir / "tradeoff.pdf"
    plt.savefig(out_path, bbox_inches="tight")
    print(f"Saved {out_path}")


def generate_tab9_scalability(data_dir: Path, output_dir: Path, **kwargs):
    """Generate LaTeX table for scalability results (Table 9)."""
    data_path = data_dir / "fig11_scalability.json"
    if not data_path.exists():
        print(f"ERROR: {data_path} not found")
        return

    with open(data_path, "rb") as f:
        data = orjson.loads(f.read())

    def fmt_size(b):
        """Format bytes to human-readable with \\, separator."""
        if b >= 1e9:
            return f"{b / 1e9:.0f}\\,GB" if b >= 10e9 else f"{b / 1e9:.1f}\\,GB"
        return f"{b / 1e6:.0f}\\,MB"

    def fmt_spans(m):
        if m >= 1000:
            return f"{m / 1000:.1f}B"
        return f"{m:.0f}M"

    lines = [
        r"\begin{table}[t]",
        r"    \centering",
        r"    \caption{Scalability on Uber. Ratio = Raw\,(gz)\,/\,Model\,(gz).}",
        r"    \label{tab:scalability}",
        r"    \footnotesize",
        r"    \setlength{\tabcolsep}{3.5pt}",
        r"    \begin{tabular}{rr rr rr rr}",
        r"        \toprule",
        r"        & & \multicolumn{2}{c}{Raw size} & \multicolumn{2}{c}{Model size} & & \\",
        r"        \cmidrule(lr){3-4} \cmidrule(lr){5-6}",
        r"        Traces & Spans & plain & gz & plain & gz & Ratio & Time\,(s) \\",
        r"        \midrule",
    ]

    for d in data:
        traces = d["traces"]
        traces_str = f"{traces // 1000}K"
        spans_str = fmt_spans(d["spans_millions"])
        raw_plain = fmt_size(d["raw_bytes"])
        raw_gz = fmt_size(d["raw_gz_bytes"])
        model_plain = fmt_size(d["model_raw_bytes"])
        model_gz = fmt_size(d["model_gz_bytes"])
        ratio = (
            d["raw_gz_bytes"] / d["model_gz_bytes"] if d["model_gz_bytes"] > 0 else 0
        )
        time_s = d["total_seconds"]
        lines.append(
            f"        {traces_str} & {spans_str} & {raw_plain} & {raw_gz} & "
            f"{model_plain} & {model_gz} & {ratio:.0f}$\\times$ & {time_s:.0f} \\\\"
        )

    lines += [
        r"        \bottomrule",
        r"    \end{tabular}",
        r"\end{table}",
    ]

    output_dir.mkdir(parents=True, exist_ok=True)
    out_path = output_dir / "scalability.tex"
    out_path.write_text("\n".join(lines) + "\n")
    print(f"Saved {out_path}")


def generate_tab_cross_dataset(
    data_dir: Path, output_dir: Path, datasets: list = None, **kwargs
):
    """Generate LaTeX table: per-metric fidelity of TPack vs Head 1:20 across datasets."""
    if not datasets:
        print("ERROR: --datasets required for tab_cross_dataset")
        return

    import glob as glob_mod
    from collections import defaultdict

    BASELINE = "1:50"
    metrics = ["rate", "error", "dur", "graph"]
    metric_labels = ["Rate", "Error", "Dur.", "Graph"]

    rows = []  # (dataset_label, approach, rate, error, dur, graph)

    for ds in datasets:
        label, paths_str = ds.split(":", 1) if ":" in ds else ("", ds)
        report_paths = []
        for p in paths_str.split(","):
            report_paths.extend([Path(ep) for ep in sorted(glob_mod.glob(p.strip()))])
        if not report_paths:
            continue

        agg = defaultdict(lambda: defaultdict(list))
        for rp in report_paths:
            report_data, experiments = load_report_data(rp)
            per_comp = extract_fidelity_per_compressor(
                report_data, experiments, weighted=False, min_count=1
            )
            for key, d in per_comp.items():
                cls = classify_experiment(d)
                if cls is None:
                    continue
                approach, alabel = cls
                if approach == "TPack":
                    app_key = "TPack"
                elif approach == "Head" and alabel == f"1:{BASELINE.split(':')[1]}":
                    app_key = f"Head {BASELINE}"
                else:
                    continue
                for m in metrics:
                    if d[m] is not None:
                        agg[app_key][m].append(d[m])

        for app in ["TPack", f"Head {BASELINE}"]:
            vals = {}
            for m in metrics:
                v = agg[app][m]
                vals[m] = np.mean(v) if v else None
            rows.append((label, app, vals))

    # Generate LaTeX
    lines = [
        r"\begin{table}[t]",
        r"    \centering",
        f"    \\caption{{Per-metric fidelity (\\%) of \\sysname vs.\\ Head {BASELINE} at comparable cost. "
        r"Unweighted mean MAPE; duration averages p50/p90/p99; graph uses structural fidelity. "
        r"Best in \textbf{bold}. --- = no error data.}",
        r"    \label{tab:cross-dataset}",
        r"    \small",
        r"    \begin{tabular}{llcccc}",
        r"        \toprule",
        r"        Dataset & Approach & Rate & Error & Dur. & Graph \\",
        r"        \midrule",
    ]

    for i in range(0, len(rows), 2):
        ds_label, _, tpack_vals = rows[i]
        _, _, head_vals = rows[i + 1]

        for j, (app_name, vals) in enumerate(
            [(r"\sysname", tpack_vals), (f"Head {BASELINE}", head_vals)]
        ):
            cells = []
            for m in metrics:
                gv = tpack_vals[m]
                hv = head_vals[m]
                v = vals[m]
                if v is None:
                    cells.append("---")
                elif gv is not None and hv is not None and v >= max(gv, hv) - 0.05:
                    cells.append(f"\\textbf{{{v:.1f}}}")
                else:
                    cells.append(f"{v:.1f}")

            prefix = (
                f"        \\multirow{{2}}{{*}}{{{ds_label}}}" if j == 0 else "        "
            )
            lines.append(f"{prefix}")
            lines.append(f"            & {app_name} & {' & '.join(cells)} \\\\")

        if i + 2 < len(rows):
            lines.append(r"        \midrule")

    lines += [
        r"        \bottomrule",
        r"    \end{tabular}",
        r"\end{table}",
    ]

    output_dir.mkdir(parents=True, exist_ok=True)
    out_path = output_dir / "cross_dataset.tex"
    out_path.write_text("\n".join(lines) + "\n")
    print(f"Saved {out_path}")


def _collect_rca_points(report_paths, rca_metric, ac_k):
    """Collect (approach, label, ac_pct, cost) points for RCA scatter plot.
    rca_metric: "trace_rca" or "micro_rank"
    ac_k: which AC@k to use (1-5)
    Aggregates across multiple reports (scenarios) per (experiment, seed)."""
    from collections import defaultdict

    agg = defaultdict(lambda: {"ac_vals": [], "costs": []})

    ac_key = f"ac{ac_k}"
    for rp in report_paths:
        report_data, experiments = load_report_data(rp)
        rca_data = report_data["reports"].get(rca_metric, {})

        # Build cost lookup from experiments
        cost_lookup = {exp.compressor_key: exp.total_cost for exp in experiments}

        for comp_key, v in rca_data.items():
            if not isinstance(v, dict) or ac_key not in v:
                continue
            ac_val = v[ac_key]["avg"] * 100  # 0/1 → 0/100%
            cost = cost_lookup.get(comp_key, 0)

            # Find matching experiment to classify
            exp_match = None
            for exp in experiments:
                if exp.compressor_key == comp_key:
                    exp_match = exp
                    break
            if exp_match is None:
                continue

            key = (
                exp_match.experiment_name,
                exp_match.iteration,
                exp_match.is_head_sampling,
            )
            agg[key]["ac_vals"].append(ac_val)
            agg[key]["costs"].append(cost)

    points = []
    for (ename, iteration, is_head), data in agg.items():
        avg_ac = np.mean(data["ac_vals"])
        avg_cost = np.mean(data["costs"])
        dummy = {"is_head": is_head, "experiment_name": ename}
        cls = classify_experiment(dummy)
        if cls is None:
            continue
        approach, label = cls
        points.append((approach, label, avg_ac, avg_cost))
    return points


def _collect_anomaly_points(report_paths):
    """Collect (approach, label, tpr_pct, cost) points for anomaly-detection
    cost-fidelity scatter. Aggregates across reports per (experiment, iteration)."""
    from collections import defaultdict

    agg = defaultdict(lambda: {"tpr_vals": [], "costs": []})

    for rp in report_paths:
        report_data, experiments = load_report_data(rp)
        ad_data = report_data["reports"].get("anomaly_detection", {})
        cost_lookup = {exp.compressor_key: exp.total_cost for exp in experiments}

        for comp_key, v in ad_data.items():
            if not isinstance(v, dict) or "detected" not in v:
                continue
            tpr = v["detected"]["avg"] * 100
            cost = cost_lookup.get(comp_key, 0)

            exp_match = None
            for exp in experiments:
                if exp.compressor_key == comp_key:
                    exp_match = exp
                    break
            if exp_match is None:
                continue

            key = (
                exp_match.experiment_name,
                exp_match.iteration,
                exp_match.is_head_sampling,
            )
            agg[key]["tpr_vals"].append(tpr)
            agg[key]["costs"].append(cost)

    points = []
    for (ename, iteration, is_head), data in agg.items():
        avg_tpr = np.mean(data["tpr_vals"])
        avg_cost = np.mean(data["costs"])
        dummy = {"is_head": is_head, "experiment_name": ename}
        cls = classify_experiment(dummy)
        if cls is None:
            continue
        approach, label = cls
        # Drop the biased-sampling reference lines (Tail-Backend, Hindsight,
        # Sifter): plot_scatter renders them as horizontal cost dashlines,
        # which clutter the TPR-cost story without adding signal here.
        if approach in {"Tail", "Hindsight", "Sifter"}:
            continue
        points.append((approach, label, avg_tpr, avg_cost))
    return points


def plot_fig_anomaly_tradeoff(data_dir: Path, output_dir: Path, datasets: list = None, **kwargs):
    """Cost-fidelity tradeoff for CUSUM anomaly detection: TPR on x-axis, cost
    on (log) y-axis. Side-by-side panels per dataset, head-rate sweep + T-Pack."""
    if not datasets:
        print("ERROR: --datasets required for anomaly_tradeoff")
        return

    import glob as glob_mod

    dataset_configs = []
    for ds in datasets:
        if ":" in ds:
            label, paths_str = ds.split(":", 1)
        else:
            label, paths_str = "", ds
        report_paths = []
        for p in paths_str.split(","):
            report_paths.extend([Path(ep) for ep in sorted(glob_mod.glob(p.strip()))])
        if report_paths:
            dataset_configs.append((label, report_paths))

    if not dataset_configs:
        print("ERROR: no datasets to plot")
        return

    with plt.rc_context(_BIG_RC):
        ncols = len(dataset_configs)
        fig, axes = plt.subplots(1, ncols, figsize=(3.5 * ncols, 3.2))
        if ncols == 1:
            axes = np.array([axes])

        for col, (label, report_paths) in enumerate(dataset_configs):
            ax = axes[col]
            points = _collect_anomaly_points(report_paths)
            plot_scatter(
                ax,
                points,
                title=label,
                xlabel="TPR (%)",
                show_title=True,
                annotate_heads=True,
                head_label_pos="right",
            )
            # Zoom the TPR axis to the high-fidelity region where the
            # action is (every compressor exceeds 85% TPR).
            ax.set_xlim(85, 100)

        for col in range(1, ncols):
            axes[col].set_ylabel("")

        add_shared_legend(
            fig, axes, fontsize=_BIG_RC["legend.fontsize"], nrows=1
        )
        out_path = output_dir / "anomaly_tradeoff.pdf"
        plt.savefig(out_path, bbox_inches="tight")
        print(f"Saved {out_path}")


def plot_fig_rca(data_dir: Path, output_dir: Path, datasets: list = None, **kwargs):
    """Fig RCA: horizontal 1×N grid of AC@k vs Cost scatter plots for TraceRCA.
    Cols: datasets (RE2-TT, RE2-OB). RE2-OB uses AC@1 (7 services), RE2-TT uses
    AC@4 (27 services)."""
    if not datasets:
        print("ERROR: --datasets required for rca")
        return

    import glob as glob_mod

    dataset_configs = []
    for ds in datasets:
        if ":" in ds:
            label, paths_str = ds.split(":", 1)
        else:
            label, paths_str = "", ds
        report_paths = []
        for p in paths_str.split(","):
            report_paths.extend([Path(ep) for ep in sorted(glob_mod.glob(p.strip()))])
        if report_paths:
            dataset_configs.append((label, report_paths))

    if not dataset_configs:
        print("ERROR: no datasets to plot")
        return

    # AC@k choice per dataset: use AC@1 for small service count, AC@3 for large
    # RE2-OB has 7 services, RE2-TT has 27
    ac_k_map = {}
    random_guess = {}
    for label, _ in dataset_configs:
        if "OB" in label:
            ac_k_map[label] = 1
            random_guess[label] = 1 / 7 * 100  # 14.3%
        else:
            ac_k_map[label] = 4
            random_guess[label] = 4 / 27 * 100  # 14.8%

    with plt.rc_context(_BIG_RC):
        metric_key, metric_name = "trace_rca", "TraceRCA"
        ncols = len(dataset_configs)
        # Height sized so the subplot plot area matches one row of fig9
        # (~2.1" per row after shared overhead). Extra height covers the
        # 1-row figure's legend strip + bottom x-labels.
        fig, axes = plt.subplots(1, ncols, figsize=(3.5 * ncols, 3.2))
        if ncols == 1:
            axes = np.array([axes])

        for col, (label, report_paths) in enumerate(dataset_configs):
            ac_k = ac_k_map[label]
            ax = axes[col]
            points = _collect_rca_points(report_paths, metric_key, ac_k)
            title = f"{label} — {metric_name} (AC@{ac_k})"
            plot_scatter(
                ax,
                points,
                title,
                xlabel=f"AC@{ac_k} (%)",
                show_title=True,
                annotate_heads=True,
                head_label_pos="right",
            )

            # Random guess vertical line (after scatter so ylim is set)
            rg = random_guess[label]
            ax.axvline(
                rg, color="#e377c2", linestyle="--", linewidth=2.0, alpha=0.85, zorder=1
            )
            ymin, ymax = ax.get_ylim()
            ax.text(
                rg,
                ymin * 1.5,
                "random\nguess",
                color="#e377c2",
                fontsize=9,
                va="bottom",
                ha="center",
                alpha=0.9,
            )

        # Only left-most subplot keeps its y-label
        for col in range(1, ncols):
            axes[col].set_ylabel("")

        add_shared_legend(fig, axes, fontsize=_BIG_RC["legend.fontsize"])
        out_path = output_dir / "rca.pdf"
        plt.savefig(out_path, bbox_inches="tight")
        print(f"Saved {out_path}")


# ── Component ablation figures (fig12-15) ──


def _setup_cost_fidelity_ax(ax, xlim=(0, 105)):
    """Standard cost-fidelity axes for all component-ablation figures."""
    ax.set_xlabel("Mean Fidelity (%)")
    ax.set_ylabel("Cost ($)")
    ax.set_yscale("log")
    ax.set_xlim(*xlim)


def _plot_head_sampling_overlay(
    ax,
    experiments,
    report_data,
    rates_to_show=None,
    fidelity_fn=None,
    annotation_fontsize=7,
):
    """Plot head-sampling points (mean±std across iterations) on an ablation scatter."""
    if rates_to_show is None:
        rates_to_show = {"1", "2", "3", "5", "10", "20", "50", "100"}
    if fidelity_fn is None:
        fidelity_fn = collect_mean_fidelity
    head_pts = {}  # rate → [(fid, cost)]
    for exp in experiments:
        if not exp.is_head_sampling:
            continue
        rate = exp.experiment_name.split("_")[0]
        if rate not in rates_to_show:
            continue
        fid = fidelity_fn(report_data, exp.compressor_key)
        if fid is None:
            continue
        head_pts.setdefault(rate, []).append((fid, exp.total_cost))

    if not head_pts:
        return

    hs = APPROACH_STYLES["Head"]
    added_label = False
    for rate, vals in sorted(head_pts.items(), key=lambda kv: int(kv[0])):
        xs, ys = zip(*vals)
        mx, my = np.mean(xs), np.mean(ys)
        label = "Head" if not added_label else None
        added_label = True
        ax.scatter(
            mx,
            my,
            marker=hs["marker"],
            s=hs["size"],
            color=hs["color"],
            zorder=hs["zorder"],
            edgecolors="black",
            linewidths=0.5,
            label=label,
        )
        ax.annotate(
            f"1:{rate}",
            (mx, my),
            xytext=(-6, -2),
            textcoords="offset points",
            fontsize=annotation_fontsize,
            color=hs["color"],
            ha="right",
            va="center",
        )


def plot_fig12_node_ablation(data_dir: Path, output_dir: Path, report=None, **kwargs):
    """Fig 12: Node ablation — leave-one-out feature removal on otel-demo.
    Shows how dropping each feature moves the TPack point in (fidelity, cost) space,
    overlaid on head-sampling baselines. Leave-one-out points are labelled with
    short codes (A, B, ...) keyed to a legend mapping to avoid label overlap
    in the cluster near Full TPack."""
    if report is None:
        print("ERROR: --report required for node_ablation")
        return
    if isinstance(report, list):
        report_data, experiments = load_and_merge_reports(report)
    else:
        report_data, experiments = load_report_data(report)

    # Short display names for each leave-one-out variant (underscore key →
    # dotted OTLP attribute name).
    LOO_DISPLAY = {
        "service_name": "service.name",
        "span_kind": "span.kind",
        "operation_name": "operation.name",
        "status_code": "status.code",
        "http_status_code": "http.status_code",
        "server_address": "server.address",
        "server_port": "server.port",
        "rpc_method": "rpc.method",
        "rpc_service": "rpc.service",
        "next_span_name": "next.span_name",
        "peer_address": "peer.address",
        "next_span_signature": "next.span_signature",
        "rpc_system": "rpc.system",
        "rpc_grpc_status_code": "rpc.grpc.status_code",
        "http_method": "http.method",
        "http_target": "http.target",
        "upstream_cluster": "upstream_cluster",
        "upstream_cluster_name": "upstream_cluster.name",
        "component": "component",
        "response_flags": "response_flags",
        "http_protocol": "http.protocol",
        "net_peer_name": "net.peer.name",
    }

    full_pt = None  # (fid, cost) for tpack_default
    add_pt = None  # (fid, cost) for tpack_add_net_peer_port
    loo = {}  # key → (fid, cost)
    for exp in experiments:
        fid = collect_mean_fidelity(report_data, exp.compressor_key)
        if fid is None:
            continue
        ename = exp.experiment_name
        if ename == "default":
            full_pt = (fid, exp.total_cost)
        elif ename == "add_net_peer_port":
            add_pt = (fid, exp.total_cost)
        elif ename.startswith("no_"):
            key = ename[len("no_") :]
            loo[key] = (fid, exp.total_cost)

    with plt.rc_context(_BIG_RC):
        fig, ax = plt.subplots(figsize=(5.5, 4.0))
        gs = APPROACH_STYLES["TPack"]

        _plot_head_sampling_overlay(
            ax, experiments, report_data, annotation_fontsize=10
        )

        # Plot only the 4 leave-one-out variants that hurt fidelity the most; the
        # other 18 cluster indistinguishably around Full TPack and add visual noise.
        top_k = 4
        ordered = sorted(loo.items(), key=lambda kv: kv[1][0])[:top_k]
        code_map = {}  # key → letter
        for idx, (key, _) in enumerate(ordered):
            code_map[key] = chr(ord("A") + idx)

        drop_color = "#1f77b4"
        first_loo = True
        for key, (fid, cost) in ordered:
            ax.scatter(
                fid,
                cost,
                marker="o",
                s=55,
                color=drop_color,
                zorder=4,
                edgecolors="black",
                linewidths=0.4,
                label="Feature ablation (drop)" if first_loo else None,
            )
            first_loo = False
            ax.annotate(
                code_map[key],
                (fid, cost),
                xytext=(-10, -3),
                textcoords="offset points",
                fontsize=11,
                fontweight="bold",
                color="#0d3d5c",
                ha="right",
            )

        # "Add-one-in" (labeled E): promote a high-cardinality attribute from
        # metadata to features. Same "feature ablation" family, distinct colour.
        add_color = "#2ca02c"
        if add_pt is not None:
            ax.scatter(
                add_pt[0],
                add_pt[1],
                marker="o",
                s=55,
                color=add_color,
                zorder=4,
                edgecolors="black",
                linewidths=0.4,
                label="Feature ablation (add)",
            )
            ax.annotate(
                "E",
                add_pt,
                xytext=(-10, -3),
                textcoords="offset points",
                fontsize=11,
                fontweight="bold",
                color="#1a5c1a",
                ha="right",
            )

        # Plot full TPack on top (rendered last so it's on top, legend listed last)
        if full_pt is not None:
            ax.scatter(
                full_pt[0],
                full_pt[1],
                marker=gs["marker"],
                s=gs["size"] * 1.3,
                color=gs["color"],
                zorder=gs["zorder"],
                edgecolors="black",
                linewidths=0.6,
                label=SYS_NAME,
            )
            ax.annotate(
                SYS_NAME,
                full_pt,
                xytext=(8, 6),
                textcoords="offset points",
                fontsize=12,
                fontweight="bold",
                color=gs["color"],
            )

        _setup_cost_fidelity_ax(ax)

        # Build code mapping string for the legend — letters cover the top-4 worst
        # dropped features; E is the add-one-in variant.
        mapping_lines = [
            f"{code_map[k]}: drop {LOO_DISPLAY.get(k, k)}" for k, _ in ordered
        ]
        if add_pt is not None:
            mapping_lines.append("E: add net.peer.port")
        mapping_text = "\n".join(mapping_lines)
        # Main legend (with approach entries) in one corner
        ax.legend(loc="upper left", fontsize=10)
        # Code mapping as a text box inside the plot (upper-right, slightly below
        # the top edge so it doesn't collide with the TPack / 1:1 annotations).
        ax.text(
            0.98,
            0.77,
            mapping_text,
            transform=ax.transAxes,
            fontsize=9.5,
            verticalalignment="top",
            horizontalalignment="right",
            multialignment="left",
            bbox=dict(
                boxstyle="round,pad=0.3",
                facecolor="white",
                edgecolor="#888888",
                linewidth=0.5,
                alpha=0.95,
            ),
        )

        plt.tight_layout()
        out_path = output_dir / "node_ablation.pdf"
        plt.savefig(out_path, bbox_inches="tight")
        print(f"Saved {out_path}")


def plot_fig13_graph_ablation(data_dir: Path, output_dir: Path, **kwargs):
    """Fig 13: Graph ablation — template vs edge mode swept across uber size.
    Y-axis is cost per billion spans (unit cost, scale-normalized).
    2 curves; marker size grows with N; each point annotated with T (unique templates)."""
    data_path = data_dir / "fig13_graph_ablation.json"
    if not data_path.exists():
        print(
            f"ERROR: {data_path} not found (run `graph-ablation` + `collect_graph_ablation` first)"
        )
        return
    with open(data_path, "rb") as f:
        data = orjson.loads(f.read())

    fig, ax = plt.subplots(figsize=(4.5, 3.2))

    for series_key, label, style_key in [
        ("edge", "Edge mode", "TPack"),
        ("template", "Template mode", "TPack-Template"),
    ]:
        pts = data[series_key]
        pts_sorted = sorted(pts, key=lambda p: p["N"])
        style = APPROACH_STYLES[style_key]
        xs = [p["fidelity"] for p in pts_sorted]
        ys = [p.get("cost_per_bspans") or p["cost"] for p in pts_sorted]
        use_cpb = pts_sorted and pts_sorted[0].get("cost_per_bspans") is not None
        xerrs = [p.get("fidelity_std", 0.0) for p in pts_sorted]
        yerrs = [
            (p.get("cost_per_bspans_std", 0.0) if use_cpb else p.get("cost_std", 0.0))
            for p in pts_sorted
        ]
        ax.plot(
            xs, ys, "-", color=style["color"], alpha=0.35, zorder=style["zorder"] - 1
        )
        ax.errorbar(
            xs,
            ys,
            xerr=xerrs,
            yerr=yerrs,
            fmt="none",
            ecolor=style["color"],
            elinewidth=0.8,
            capsize=2,
            alpha=0.7,
            zorder=style["zorder"] - 1,
        )
        for p in pts_sorted:
            y = p.get("cost_per_bspans") or p["cost"]
            ax.scatter(
                p["fidelity"],
                y,
                marker=style["marker"],
                s=style["size"],
                color=style["color"],
                zorder=style["zorder"],
                edgecolors="black",
                linewidths=0.5,
                label=label if p is pts_sorted[0] else None,
            )
            n_str = f"{p['N'] // 1000}k" if p["N"] >= 1000 else str(p["N"])
            t_raw = p.get("templates")
            e_raw = p.get("edges")
            if series_key == "template" and t_raw is not None:
                t_str = f"{t_raw / 1000:.1f}k" if t_raw >= 1000 else str(t_raw)
                # Nudge the top labels down so they don't collide with neighbours.
                dy = {20000: -5, 50000: -10}.get(p["N"], 0)
                # N on the left of the marker, T on the right.
                ax.annotate(
                    f"N={n_str}",
                    (p["fidelity"], y),
                    xytext=(-10, dy),
                    textcoords="offset points",
                    fontsize=6,
                    ha="right",
                    va="center",
                    color=style["color"],
                )
                ax.annotate(
                    f"T={t_str}",
                    (p["fidelity"], y),
                    xytext=(10, dy),
                    textcoords="offset points",
                    fontsize=6,
                    ha="left",
                    va="center",
                    color=style["color"],
                )
            elif series_key == "edge" and e_raw is not None:
                e_str = f"{e_raw / 1000:.1f}k" if e_raw >= 1000 else str(e_raw)
                # N on the left of the marker, E on the right.
                ax.annotate(
                    f"N={n_str}",
                    (p["fidelity"], y),
                    xytext=(-10, 0),
                    textcoords="offset points",
                    fontsize=6,
                    ha="right",
                    va="center",
                    color=style["color"],
                )
                ax.annotate(
                    f"E={e_str}",
                    (p["fidelity"], y),
                    xytext=(10, 0),
                    textcoords="offset points",
                    fontsize=6,
                    ha="left",
                    va="center",
                    color=style["color"],
                )
            else:
                ax.annotate(
                    f"N={n_str}",
                    (p["fidelity"], y),
                    xytext=(-10, 0),
                    textcoords="offset points",
                    fontsize=6,
                    ha="right",
                    va="center",
                    color=style["color"],
                )

    ax.set_xlabel("Mean Fidelity (%)")
    ax.set_ylabel("Cost per 1B spans ($)")
    ax.set_yscale("log")
    ax.set_xlim(50, 100)
    ax.set_yticks([0.05, 0.1, 0.2, 0.5])
    ax.get_yaxis().set_major_formatter(ticker.FormatStrFormatter("%.2f"))
    ax.get_yaxis().set_minor_formatter(ticker.NullFormatter())

    ax.legend(loc="upper left", fontsize=8)

    plt.tight_layout()
    out_path = output_dir / "graph_ablation.pdf"
    plt.savefig(out_path, bbox_inches="tight")
    print(f"Saved {out_path}")


def plot_fig14_root_duration_ablation(
    data_dir: Path, output_dir: Path, report=None, **kwargs
):
    """Fig 14: Root-duration ablation — GMM components K=1..5.
    X axis = duration fidelity (p50/p90/p99 averaged) since this knob only
    affects the root timing model, not rate/error/graph."""
    if report is None:
        print("ERROR: --report required for root_duration_ablation")
        return
    if isinstance(report, list):
        report_data, experiments = load_and_merge_reports(report)
    else:
        report_data, experiments = load_report_data(report)

    pts = []  # (K, fid, cost)
    for exp in experiments:
        fid = collect_duration_fidelity(report_data, exp.compressor_key)
        if fid is None:
            continue
        ename = exp.experiment_name
        if ename.startswith("gmm"):
            try:
                K = int(ename[3:])
                pts.append((K, fid, exp.total_cost))
            except ValueError:
                continue
    pts.sort(key=lambda p: p[0])
    if not pts:
        print("ERROR: no gmm* compressors found in report")
        return

    fig, ax = plt.subplots(figsize=(4.5, 3.2))
    gs = APPROACH_STYLES["TPack"]

    _plot_head_sampling_overlay(
        ax, experiments, report_data, fidelity_fn=collect_duration_fidelity
    )

    xs = [p[1] for p in pts]
    ys = [p[2] for p in pts]
    ax.plot(xs, ys, "-", color=gs["color"], alpha=0.4, zorder=gs["zorder"] - 1)
    for K, fid, cost in pts:
        ax.scatter(
            fid,
            cost,
            marker=gs["marker"],
            s=gs["size"],
            color=gs["color"],
            zorder=gs["zorder"],
            edgecolors="black",
            linewidths=0.5,
            label="TPack (GMM sweep)" if K == 1 else None,
        )
        ax.annotate(
            f"K={K}",
            (fid, cost),
            xytext=(6, 5),
            textcoords="offset points",
            fontsize=8,
            color=gs["color"],
        )

    ax.set_xlabel("Duration Fidelity (%)")
    ax.set_ylabel("Cost ($)")
    ax.set_yscale("log")
    ax.set_xlim(0, 105)
    ax.legend(loc="best", fontsize=8)
    plt.tight_layout()
    out_path = output_dir / "root_duration_ablation.pdf"
    plt.savefig(out_path, bbox_inches="tight")
    print(f"Saved {out_path}")


def plot_fig15_reject_sampling(data_dir: Path, output_dir: Path, report=None, **kwargs):
    """Fig 15: Reject-sampling ablation — with vs without bounds-based reject
    sampling on root/child timing (otel-demo, template mode). X axis = duration
    fidelity."""
    if report is None:
        print("ERROR: --report required for reject_sampling")
        return
    if isinstance(report, list):
        report_data, experiments = load_and_merge_reports(report)
    else:
        report_data, experiments = load_report_data(report)

    # Aggregate per-variant points across seeds.
    pts = {}  # ename -> list of (fid, cost)
    for exp in experiments:
        fid = collect_duration_fidelity(report_data, exp.compressor_key)
        if fid is None:
            continue
        pts.setdefault(exp.experiment_name, []).append((fid, exp.total_cost))

    fig, ax = plt.subplots(figsize=(4.5, 3.2))

    _plot_head_sampling_overlay(
        ax, experiments, report_data, fidelity_fn=collect_duration_fidelity
    )

    # Distinct marker + legend entry per variant.
    variant_styles = {
        "bounds": {
            "marker": "*",
            "label": "TPack (w/ reject sampling)",
            "color": "#d62728",
            "size": 180,
        },
        "nobounds": {
            "marker": "X",
            "label": "TPack (w/o reject sampling)",
            "color": "#ff7f0e",
            "size": 90,
        },
    }
    for ename, style in variant_styles.items():
        if ename not in pts:
            continue
        vals = pts[ename]
        xs, ys = zip(*vals)
        mx, my = float(np.mean(xs)), float(np.mean(ys))
        sx, sy = float(np.std(xs)), float(np.std(ys))
        ax.errorbar(
            mx,
            my,
            xerr=sx,
            yerr=sy,
            fmt="none",
            color=style["color"],
            zorder=8,
            **ERRORBAR_STYLE,
        )
        ax.scatter(
            mx,
            my,
            marker=style["marker"],
            s=style["size"],
            color=style["color"],
            zorder=9,
            edgecolors="black",
            linewidths=0.6,
            label=style["label"],
        )

    ax.set_xlabel("Duration Fidelity (%)")
    ax.set_ylabel("Cost ($)")
    ax.set_yscale("log")
    ax.set_xlim(0, 105)
    ax.legend(loc="best", fontsize=8)
    plt.tight_layout()
    out_path = output_dir / "reject_sampling.pdf"
    plt.savefig(out_path, bbox_inches="tight")
    print(f"Saved {out_path}")


# ── Main ──

MODES = {
    "tradeoff": plot_fig3_tradeoff,
    "cost_fidelity": plot_fig8_cost_fidelity,
    "query_fidelity": plot_fig9_query_fidelity,
    "scalability": plot_fig11_scalability,
    "node_ablation": plot_fig12_node_ablation,
    "graph_ablation": plot_fig13_graph_ablation,
    "root_duration_ablation": plot_fig14_root_duration_ablation,
    "reject_sampling": plot_fig15_reject_sampling,
    "tab_scalability": generate_tab9_scalability,
    "tab_cross_dataset": generate_tab_cross_dataset,
    "rca": plot_fig_rca,
    "anomaly_tradeoff": plot_fig_anomaly_tradeoff,
}


def main():
    all_modes = list(MODES.keys()) + ["all"]
    parser = argparse.ArgumentParser(description="Generate paper figures")
    parser.add_argument(
        "--mode",
        required=True,
        nargs="+",
        help=f"Figure(s) to generate: {', '.join(all_modes)}",
    )
    parser.add_argument(
        "--data-dir", type=Path, default=Path("data/paper"), help="Raw data directory"
    )
    parser.add_argument(
        "--paper-dir",
        type=Path,
        default=Path("output/paper-figures"),
        help="Output directory for figures and tables",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        help="Override output directory (ignores --paper-dir)",
    )
    parser.add_argument(
        "--report", type=Path, nargs="+", help="Path(s) to report.json files"
    )
    parser.add_argument(
        "--rca-report",
        type=Path,
        help="Path to RE2-TT report.json (for fig9 RCA subplot)",
    )
    parser.add_argument(
        "--datasets",
        nargs="+",
        help="Dataset specs for fig8: 'Label:path1,path2,...' (supports globs)",
    )
    args = parser.parse_args()

    modes = list(MODES.keys()) if "all" in args.mode else args.mode
    for mode in modes:
        if mode not in MODES:
            parser.error(f"Unknown mode: {mode}")

    for mode in modes:
        func = MODES[mode]
        output_dir = args.output_dir if args.output_dir else args.paper_dir
        output_dir.mkdir(parents=True, exist_ok=True)
        func(
            data_dir=args.data_dir,
            output_dir=output_dir,
            report=args.report,
            rca_report=args.rca_report,
            datasets=args.datasets,
        )


if __name__ == "__main__":
    main()
