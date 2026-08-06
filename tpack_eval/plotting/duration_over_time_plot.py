"""Duration over time visualization module."""

import json
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np

from tpack_eval.plotting.data import ReportParser
from tpack_eval.plotting.scatter_plot_utils import (
    build_filter_title,
    format_plot_axes,
    group_experiments_by_name,
    plot_experiment_groups,
    plot_head_sampling_points,
    setup_plot_style,
)

PERCENTILES = ["p50", "p90", "p99"]


def extract_duration_data_by_filter(
    report_data, experiments, filter_level, min_count=0
):
    """
    Extract duration over time data for a specific filter level,
    averaging across all (group, percentile) pairs from p50/p90/p99.

    Args:
        report_data: Full report JSON data
        experiments: List of ExperimentData from ReportParser
        filter_level: 0 (all only), 1 (single dimension), 2+ (combinations)
        min_count: Minimum span count to include a group (0 = no filter)

    Returns:
        List of data points, one per compressor: [{
            'compressor': str,
            'group': str,
            'mape_fidelity': float,
            'count': int,
            'total_cost': float
        }]
    """
    reports = report_data.get("reports", {})
    cost_lookup = {exp.compressor_key: exp.total_cost for exp in experiments}

    # Collect all compressor keys across percentile sections
    all_compressors = set()
    for pct in PERCENTILES:
        section = reports.get(f"duration_over_time_{pct}", {})
        all_compressors.update(section.keys())

    data_points = []
    for compressor_key in all_compressors:
        cost = cost_lookup.get(compressor_key, 0)
        mape_values = []
        weight_values = []

        for pct in PERCENTILES:
            section = reports.get(f"duration_over_time_{pct}", {})
            groups = section.get(compressor_key, {})

            if filter_level == 0:
                if "all" in groups:
                    metrics = groups["all"]
                    count = metrics.get("count", 0)
                    if min_count > 0 and count < min_count:
                        continue
                    mape_values.append(metrics.get("mape_fidelity", 0))
                    weight_values.append(count)
            else:
                for group_key, metrics in groups.items():
                    include = False
                    if filter_level == 1:
                        include = (
                            ":" in group_key
                            and "!@#" not in group_key
                            and group_key != "all"
                        )
                    elif filter_level >= 2:
                        include = group_key.count("!@#") == filter_level - 1

                    if include:
                        count = metrics.get("count", 0)
                        if min_count > 0 and count < min_count:
                            continue
                        mape_values.append(metrics.get("mape_fidelity", 0))
                        weight_values.append(count)

        if mape_values:
            avg = float(np.mean(mape_values))
            data_points.append(
                {
                    "compressor": compressor_key,
                    "group": f"average_of_{len(mape_values)}_queries",
                    "mape_fidelity": avg,
                    "count": len(mape_values),
                    "total_cost": cost,
                }
            )

    return data_points


def plot_duration_scatter(data_points, metric, filter_level, output_dir, min_count=0):
    """
    Create scatter plot of fidelity vs cost.

    Args:
        data_points: List of data points from extract_duration_data_by_filter
        metric: 'mape_fidelity' or 'cosine_fidelity'
        filter_level: 0, 1, or 2
        output_dir: Directory to save plot
        min_count: Minimum span count filter applied (for filename)
    """
    if not data_points:
        print(f"No data points for filter level {filter_level}, metric {metric}")
        return

    setup_plot_style()

    experiment_groups, head_sampling_points = group_experiments_by_name(
        data_points, metric
    )

    plot_experiment_groups(experiment_groups)
    plot_head_sampling_points(head_sampling_points)

    metric_name = "MAPE" if metric == "mape_fidelity" else "Cosine Similarity"
    title = build_filter_title("Duration Over Time", metric, filter_level)
    y_label = f"{metric_name} Fidelity (%)"
    format_plot_axes(title, y_label)

    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)

    metric_short = "mape" if metric == "mape_fidelity" else "cosine"
    filter_suffix = f"{filter_level}_filter" if filter_level <= 1 else f"{filter_level}_filters"
    min_count_suffix = f"_min{min_count}" if min_count > 0 else ""
    filename = f"duration_over_time_{filter_suffix}_{metric_short}{min_count_suffix}.png"

    plt.tight_layout()
    plt.savefig(output_path / filename, dpi=300, bbox_inches="tight")
    plt.close()

    print(f"Saved: {output_path / filename}")


def generate_all_duration_plots(report_path, output_dir="./plots", cost_config=None, min_count=0):
    """
    Generate duration over time scatter plots.
    Each point is the simple average across all (group, percentile) pairs.

    Args:
        report_path: Path to the report JSON file
        output_dir: Directory to save plots
        cost_config: Optional CostConfig for cost calculation
        min_count: Minimum span count to include a group (0 = no filter)
    """
    with open(report_path) as f:
        report_data = json.load(f)

    parser = ReportParser(cost_config=cost_config)
    experiments = parser.parse_report(report_path)

    if not experiments:
        print("No experiments found in report")
        return

    print(f"Found {len(experiments)} experiments")

    for filter_level in range(3):
        data_points = extract_duration_data_by_filter(
            report_data, experiments, filter_level, min_count=min_count,
        )
        print(f"Filter level {filter_level}: {len(data_points)} data points")

        for metric in ["mape_fidelity"]:  # "cosine_fidelity"
            plot_duration_scatter(
                data_points, metric, filter_level, output_dir,
                min_count=min_count,
            )

    print(f"All duration over time plots generated in {output_dir}")
