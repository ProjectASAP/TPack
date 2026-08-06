"""Operation fidelity visualization module for Operation F1 and Operation Pair F1."""

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


def extract_operation_data(report_data, experiments, metric_name):
    """
    Extract operation data for a specific metric.

    Args:
        report_data: Full report JSON data
        experiments: List of ExperimentData from ReportParser
        metric_name: 'operation_f1' or 'operation_pair_f1'

    Returns:
        List of data points: [{
            'compressor': str,
            'fidelity': float,
            'total_cost': float
        }]
    """
    if "reports" not in report_data or "operation" not in report_data["reports"]:
        return []

    operation_report = report_data["reports"]["operation"]

    # Create lookup for cost by compressor key
    cost_lookup = {}
    for exp in experiments:
        cost_lookup[exp.compressor_key] = exp.total_cost

    data_points = []
    for compressor_key, metrics in operation_report.items():
        cost = cost_lookup.get(compressor_key, 0)

        # Extract the specific metric
        metric_data = metrics.get(metric_name, {})
        avg_value = metric_data.get("avg", 0.0)

        # Convert to percentage (multiply by 100)
        fidelity = avg_value * 100

        data_points.append(
            {
                "compressor": compressor_key,
                "fidelity": fidelity,
                "total_cost": cost,
            }
        )

    return data_points


def plot_operation_scatter(data_points, metric_name, output_dir):
    """
    Create scatter plot of operation fidelity vs cost.

    Args:
        data_points: List of data points from extract_operation_data
        metric_name: 'operation_f1' or 'operation_pair_f1'
        output_dir: Directory to save plot
    """
    if not data_points:
        print(f"No data points for {metric_name}")
        return

    # Setup plot
    setup_plot_style()

    # Group data by duration for TPack experiments
    experiment_groups, head_sampling_points = group_experiments_by_name(
        data_points, "fidelity"
    )

    # Plot TPack duration groups and head sampling
    plot_experiment_groups(experiment_groups)
    plot_head_sampling_points(head_sampling_points)

    # Format axes, title, labels, and grid
    if metric_name == "operation_f1":
        title = "Operation F1 Fidelity vs Cost"
        y_label = "Operation F1 Fidelity (%)"
        filename = "operation_f1.png"
    else:  # operation_pair_f1
        title = "Operation Pair F1 Fidelity vs Cost"
        y_label = "Operation Pair F1 Fidelity (%)"
        filename = "operation_pair_f1.png"

    format_plot_axes(title, y_label)

    # Save
    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)

    plt.tight_layout()
    plt.savefig(output_path / filename, dpi=300, bbox_inches="tight")
    plt.close()

    print(f"Saved: {output_path / filename}")


def generate_all_operation_plots(report_path, output_dir="./plots", cost_config=None):
    """
    Generate operation scatter plots for both Operation F1 and Operation Pair F1.

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

    # Generate plots for both Operation F1 and Operation Pair F1
    for metric_name in ["operation_f1", "operation_pair_f1"]:
        data_points = extract_operation_data(report_data, experiments, metric_name)
        print(f"{metric_name}: {len(data_points)} data points")

        if data_points:
            plot_operation_scatter(data_points, metric_name, output_dir)

    print(f"All operation plots generated in {output_dir}")
