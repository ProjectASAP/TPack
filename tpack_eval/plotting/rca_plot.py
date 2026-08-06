"""RCA (Root Cause Analysis) visualization module for TraceRCA and MicroRank."""

import json
from pathlib import Path

import matplotlib.pyplot as plt

from tpack_eval.plotting.data import ReportParser
from tpack_eval.plotting.scatter_plot_utils import (
    format_plot_axes,
    group_experiments_by_name,
    plot_experiment_groups,
    plot_head_sampling_points,
    setup_plot_style,
)


def extract_rca_data(report_data, experiments, metric_name):
    """
    Extract RCA data for a specific metric (tracerca or microrank).

    Args:
        report_data: Full report JSON data
        experiments: List of ExperimentData from ReportParser
        metric_name: 'tracerca' or 'microrank'

    Returns:
        List of data points: [{
            'compressor': str,
            'avg5_fidelity': float,
            'total_cost': float
        }]
    """
    report_key = "trace_rca" if metric_name == "tracerca" else "micro_rank"

    if "reports" not in report_data or report_key not in report_data["reports"]:
        return []

    rca_report = report_data["reports"][report_key]

    # Create lookup for cost by compressor key
    cost_lookup = {}
    for exp in experiments:
        cost_lookup[exp.compressor_key] = exp.total_cost

    data_points = []
    for compressor_key, metrics in rca_report.items():
        cost = cost_lookup.get(compressor_key, 0)

        # Extract avg5 metric (may be nested as {"avg": value})
        avg5_raw = metrics.get("avg5", 0.0)
        avg5 = avg5_raw["avg"] if isinstance(avg5_raw, dict) else avg5_raw

        # Convert to percentage (multiply by 100)
        avg5_fidelity = avg5 * 100

        data_points.append(
            {
                "compressor": compressor_key,
                "avg5_fidelity": avg5_fidelity,
                "total_cost": cost,
            }
        )

    return data_points


def plot_rca_ranking_scatter(data_points, metric_name, output_dir):
    """
    Create scatter plot of RCA AC@5 fidelity vs cost.

    Args:
        data_points: List of data points from extract_rca_data
        metric_name: 'tracerca' or 'microrank'
        output_dir: Directory to save plot
    """
    if not data_points:
        print(f"No Kendall tau data points for {metric_name}")
        return

    setup_plot_style()

    experiment_groups, head_sampling_points = group_experiments_by_name(
        data_points, "kendall_tau_fidelity"
    )

    plot_experiment_groups(experiment_groups)
    plot_head_sampling_points(head_sampling_points)

    metric_display = "TraceRCA" if metric_name == "tracerca" else "MicroRank"
    title = f"{metric_display} Kendall τ vs Cost"
    y_label = f"{metric_display} Kendall τ (%)"
    format_plot_axes(title, y_label)

    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)

    filename = f"rca_{metric_name}_kendall_tau.png"

    plt.tight_layout()
    plt.savefig(output_path / filename, dpi=300, bbox_inches="tight")
    plt.close()

    print(f"Saved: {output_path / filename}")


def plot_rca_scatter(data_points, metric_name, output_dir):
    """
    Create scatter plot of RCA fidelity vs cost.

    Args:
        data_points: List of data points from extract_rca_data
        metric_name: 'tracerca' or 'microrank'
        output_dir: Directory to save plot
    """
    if not data_points:
        print(f"No data points for {metric_name}")
        return

    # Setup plot
    setup_plot_style()

    # Group data by duration for TPack experiments
    experiment_groups, head_sampling_points = group_experiments_by_name(
        data_points, "avg5_fidelity"
    )

    # Plot TPack duration groups and head sampling
    plot_experiment_groups(experiment_groups)
    plot_head_sampling_points(head_sampling_points)

    # Format axes, title, labels, and grid
    metric_display = "TraceRCA" if metric_name == "tracerca" else "MicroRank"
    title = f"{metric_display} Avg@5 Fidelity vs Cost"
    y_label = f"{metric_display} Avg@5 Fidelity (%)"
    format_plot_axes(title, y_label)

    # Save
    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)

    filename = f"rca_{metric_name}_avg5.png"

    plt.tight_layout()
    plt.savefig(output_path / filename, dpi=300, bbox_inches="tight")
    plt.close()

    print(f"Saved: {output_path / filename}")


def generate_aggregate_rca_plots(report_paths, output_dir="./plots", cost_config=None, dataset_label="RE2"):
    """
    Generate aggregate RCA scatter plots from multiple report files.
    Combines data from all reports into a single plot per metric.

    Args:
        report_paths: List of paths to report JSON files
        output_dir: Directory to save plots
        cost_config: Optional CostConfig for cost calculation
        dataset_label: Label for plot titles (e.g. "RE2-TT", "RE2-OB")
    """
    from tpack_eval.plotting.data import ReportParser

    parser = ReportParser(cost_config=cost_config)

    for rca_metric in ["tracerca", "microrank"]:
        for value_metric, extract_fn, plot_fn, suffix in [
            ("avg5_fidelity", extract_rca_data, plot_rca_scatter, "avg5"),
        ]:
            all_points = []
            for rpath in report_paths:
                with open(rpath) as f:
                    report_data = json.load(f)
                experiments = parser.parse_report(rpath)
                points = extract_fn(report_data, experiments, rca_metric)
                all_points.extend(points)

            if not all_points:
                print(f"No data for {dataset_label} {rca_metric} {suffix}")
                continue

            # Plot with paper-friendly style
            plt.figure(figsize=(8, 5))
            plt.style.use("default")
            plt.rcParams.update({
                "font.size": 11,
                "axes.labelsize": 13,
                "axes.titlesize": 15,
                "xtick.labelsize": 11,
                "ytick.labelsize": 11,
                "legend.fontsize": 10,
            })

            experiment_groups, head_sampling_points = group_experiments_by_name(
                all_points, value_metric
            )

            plot_experiment_groups(experiment_groups)
            plot_head_sampling_points(head_sampling_points)

            metric_display = "TraceRCA" if rca_metric == "tracerca" else "MicroRank"
            title = f"{dataset_label}: {metric_display} AC@5 vs Cost"
            y_label = f"{metric_display} AC@5 (%)"
            format_plot_axes(title, y_label)
            # Override legend size for paper
            plt.legend(loc="best", frameon=True, fancybox=True, shadow=True, fontsize=10)

            output_path = Path(output_dir)
            output_path.mkdir(parents=True, exist_ok=True)
            filename = f"rca_{dataset_label.lower()}_{rca_metric}_{suffix}.png"

            plt.tight_layout()
            plt.savefig(output_path / filename, dpi=300, bbox_inches="tight")
            plt.close()
            print(f"Saved: {output_path / filename}")

    print(f"Aggregate RCA plots generated in {output_dir}")


def generate_all_rca_plots(report_path, output_dir="./plots", cost_config=None):
    """
    Generate RCA scatter plots for both TraceRCA and MicroRank.

    Args:
        report_path: Path to the report JSON file
        output_dir: Directory to save plots
        cost_config: Optional CostConfig for cost calculation
    """
    # Load report data
    with open(report_path) as f:
        report_data = json.load(f)

    # Use ReportParser to get experiment data with costs
    parser = ReportParser(cost_config=cost_config)
    experiments = parser.parse_report(report_path)

    if not experiments:
        print("No experiments found in report")
        return

    print(f"Found {len(experiments)} experiments")

    # Generate plots for both TraceRCA and MicroRank
    for metric_name in ["tracerca", "microrank"]:
        # Avg@5 plots
        data_points = extract_rca_data(report_data, experiments, metric_name)
        print(f"{metric_name} avg5: {len(data_points)} data points")
        if data_points:
            plot_rca_scatter(data_points, metric_name, output_dir)

    print(f"All RCA plots generated in {output_dir}")
