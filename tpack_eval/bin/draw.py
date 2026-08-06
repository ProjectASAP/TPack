import argparse
import json
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
import seaborn as sns

from tpack_eval.plotting.data import CostConfig
from tpack_eval.plotting.duration_over_time_plot import generate_all_duration_plots
from tpack_eval.plotting.error_over_time_plot import generate_all_error_plots
from tpack_eval.plotting.graph_plot import generate_all_graph_plots
from tpack_eval.plotting.operation_plot import generate_all_operation_plots
from tpack_eval.plotting.rate_over_time_plot import generate_all_rate_plots
from tpack_eval.plotting.offset_analysis_plot import generate_all_offset_analysis_plots
from tpack_eval.plotting.rca_plot import generate_aggregate_rca_plots, generate_all_rca_plots


# Hardcoded data arrays removed - now using JSON data source


def extract_mape_data_for_heatmap(report_data, compressor_key):
    """Extract MAPE data from the JSON report for a specific compressor."""
    if "reports" not in report_data or "duration" not in report_data["reports"]:
        raise ValueError("Invalid report format: missing duration data")

    duration_data = report_data["reports"]["duration"]

    # Find the matching compressor (could be part of a compound key like "app_compressor")
    matching_keys = [key for key in duration_data.keys() if compressor_key in key]
    if not matching_keys:
        available_keys = list(duration_data.keys())
        raise ValueError(
            f"Compressor '{compressor_key}' not found. Available: {available_keys}"
        )

    if len(matching_keys) > 1:
        print(f"Warning: Multiple matches for '{compressor_key}': {matching_keys}")
        print(f"Using first match: {matching_keys[0]}")

    compressor_data = duration_data[matching_keys[0]]

    # Extract MAPE metrics - pattern: depth_{X}_p{Y}_mape
    mape_data = {}
    for metric_name, metric_value in compressor_data.items():
        if "_mape" in metric_name and "depth_" in metric_name:
            parts = metric_name.split("_")

            # Find depth and percentile
            depth_idx = None
            percentile_idx = None

            for i, part in enumerate(parts):
                if part == "depth" and i + 1 < len(parts):
                    try:
                        depth_idx = int(parts[i + 1])
                    except ValueError:
                        continue
                elif part.startswith("p") and part[1:].isdigit():
                    percentile_idx = int(part[1:])

            if depth_idx is not None and percentile_idx is not None:
                if depth_idx not in mape_data:
                    mape_data[depth_idx] = {}
                mape_data[depth_idx][percentile_idx] = metric_value["avg"]

    return mape_data, matching_keys[0]


def extract_mape_data_for_status_code_heatmap(report_data, compressor_key):
    """Extract MAPE data for status codes from the JSON report for a specific compressor."""
    if "reports" not in report_data or "duration" not in report_data["reports"]:
        raise ValueError("Invalid report format: missing duration data")

    duration_data = report_data["reports"]["duration"]

    # Find the matching compressor (could be part of a compound key like "app_compressor")
    matching_keys = [key for key in duration_data.keys() if compressor_key in key]
    if not matching_keys:
        available_keys = list(duration_data.keys())
        raise ValueError(
            f"Compressor '{compressor_key}' not found. Available: {available_keys}"
        )

    if len(matching_keys) > 1:
        print(f"Warning: Multiple matches for '{compressor_key}': {matching_keys}")
        print(f"Using first match: {matching_keys[0]}")

    compressor_data = duration_data[matching_keys[0]]

    # Extract MAPE metrics - pattern: http.status_code_{code}_p{Y}_mape
    mape_data = {}
    for metric_name, metric_value in compressor_data.items():
        if "_mape" in metric_name and "http.status_code_" in metric_name:
            # Pattern: http.status_code_{code}_p{percentile}_mape
            # When split by "_": ['http.status', 'code', '{code}', 'p{percentile}', 'mape']
            parts = metric_name.split("_")

            if len(parts) >= 5:
                # Status code is at index 2 (after 'http.status' and 'code')
                status_code = parts[2]

                # Percentile is at index 3 (starts with 'p')
                percentile_part = parts[3]
                if percentile_part.startswith("p") and percentile_part[1:].isdigit():
                    percentile_idx = int(percentile_part[1:])

                    if status_code not in mape_data:
                        mape_data[status_code] = {}
                    mape_data[status_code][percentile_idx] = metric_value["avg"]

    return mape_data, matching_keys[0]


def create_mape_heatmap(mape_data, compressor_name, output_dir):
    """Create a 2D heatmap of MAPE values across depth and percentiles."""

    # Define expected depths and percentiles
    depths = list(range(5))  # 0, 1, 2, 3, 4
    percentiles = [0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100]

    # Create 2D array for heatmap
    heatmap_data = np.full((len(depths), len(percentiles)), np.nan)

    # Fill the array with available data
    for depth_idx, depth in enumerate(depths):
        if depth in mape_data:
            for perc_idx, percentile in enumerate(percentiles):
                if percentile in mape_data[depth]:
                    heatmap_data[depth_idx, perc_idx] = (
                        100 - mape_data[depth][percentile]
                    )

    # Create the heatmap
    plt.figure(figsize=(12, 8))

    # Use a colormap where lower values (better MAPE) are green
    mask = np.isnan(heatmap_data)

    sns.heatmap(
        heatmap_data,
        annot=True,
        fmt=".1f",
        cmap="RdYlGn",  # Red-Yellow-Green reversed (lower is better)
        mask=mask,
        xticklabels=[f"p{p}" for p in percentiles],
        yticklabels=[f"Depth {d}" for d in depths],
        square=True,
        cbar=False,
        vmin=0,
        vmax=100,
    )

    plt.title(
        f"MAPE Heatmap: {compressor_name}\n(Higher values = Better fidelity)",
        fontsize=16,
        fontweight="bold",
        pad=20,
    )
    plt.xlabel("Percentiles", fontsize=14, fontweight="bold")
    plt.ylabel("Span Depth", fontsize=14, fontweight="bold")

    # Improve layout
    plt.tight_layout()

    # Save the plot
    output_path = Path(output_dir) / f"{compressor_name}_mape_heatmap.png"
    output_path.parent.mkdir(parents=True, exist_ok=True)

    plt.savefig(
        output_path,
        dpi=300,
        bbox_inches="tight",
        format="png",
        facecolor="white",
        edgecolor="none",
    )

    print(f"Saved MAPE heatmap to: {output_path.resolve()}")
    plt.close()

    return output_path


def create_status_code_mape_heatmap(mape_data, compressor_name, output_dir):
    """Create a 2D heatmap of MAPE values across HTTP status codes and percentiles."""

    # Get unique status codes and sort them
    status_codes = sorted(mape_data.keys())
    percentiles = [0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100]

    # Create 2D array for heatmap
    heatmap_data = np.full((len(status_codes), len(percentiles)), np.nan)

    # Fill the array with available data
    for code_idx, status_code in enumerate(status_codes):
        if status_code in mape_data:
            for perc_idx, percentile in enumerate(percentiles):
                if percentile in mape_data[status_code]:
                    heatmap_data[code_idx, perc_idx] = (
                        100 - mape_data[status_code][percentile]
                    )

    # Create the heatmap
    plt.figure(figsize=(12, 8))

    # Use a colormap where lower values (better MAPE) are green
    mask = np.isnan(heatmap_data)

    # Create labels for status codes (use "empty" for empty string)
    status_code_labels = [code if code else "empty" for code in status_codes]

    sns.heatmap(
        heatmap_data,
        annot=True,
        fmt=".1f",
        cmap="RdYlGn",  # Red-Yellow-Green reversed (lower is better)
        mask=mask,
        xticklabels=[f"p{p}" for p in percentiles],
        yticklabels=status_code_labels,
        square=True,
        cbar=False,
        vmin=0,
        vmax=100,
    )

    plt.title(
        f"MAPE Heatmap by HTTP Status Code: {compressor_name}\n(Higher values = Better fidelity)",
        fontsize=16,
        fontweight="bold",
        pad=20,
    )
    plt.xlabel("Percentiles", fontsize=14, fontweight="bold")
    plt.ylabel("HTTP Status Code", fontsize=14, fontweight="bold")

    # Improve layout
    plt.tight_layout()

    # Save the plot
    output_path = Path(output_dir) / f"{compressor_name}_status_code_mape_heatmap.png"
    output_path.parent.mkdir(parents=True, exist_ok=True)

    plt.savefig(
        output_path,
        dpi=300,
        bbox_inches="tight",
        format="png",
        facecolor="white",
        edgecolor="none",
    )

    print(f"Saved status code MAPE heatmap to: {output_path.resolve()}")
    plt.close()

    return output_path


def main():
    # Parse command line arguments
    parser = argparse.ArgumentParser(
        description="Generate visualizations from TPack reports",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--mode",
        type=str,
        required=True,
        choices=[
            "operation",
            "graph",
            "heatmap",
            "heatmap_status_code",
            "error_over_time",
            "rate_over_time",
            "duration_over_time",
            "rca",
            "rca_aggregate",
            "offset_analysis",
        ],
        help="Visualization mode: operation (Operation F1 fidelity), graph (Graph fidelity), heatmap (depth heatmap), heatmap_status_code (HTTP status code heatmap), error_over_time (error over time fidelity vs cost), rate_over_time (rate over time fidelity vs cost), duration_over_time (duration over time fidelity vs cost), or rca (TraceRCA and MicroRank fidelity vs cost)",
    )
    parser.add_argument(
        "--input",
        type=str,
        nargs="+",
        default=["output/report.json"],
        help="Path(s) to JSON report file(s). Multiple files for rca_aggregate mode.",
    )
    parser.add_argument(
        "--label",
        type=str,
        default="RE2",
        help="Dataset label for aggregate plot titles (default: RE2)",
    )
    parser.add_argument(
        "--output-dir",
        type=str,
        default="output/visualizations",
        help="Output directory for generated plots (default: output/visualizations)",
    )
    parser.add_argument(
        "--compressor",
        type=str,
        help="Compressor name to visualize (required for heatmap modes)",
    )
    parser.add_argument(
        "--min-count",
        type=int,
        default=0,
        help="Minimum span count to include a group in duration plots (default: 0)",
    )
    args = parser.parse_args()

    cost_config = CostConfig()

    if args.mode == "heatmap":
        # Heatmap mode - requires input JSON and compressor
        if not args.input:
            parser.error("--input is required for heatmap mode")
        if not args.compressor:
            parser.error("--compressor is required for heatmap mode")

        # Load the JSON report
        try:
            with open(args.input[0]) as f:
                report_data = json.load(f)
        except FileNotFoundError:
            print(f"Error: Could not find input file: {args.input}")
            return 1
        except json.JSONDecodeError as e:
            print(f"Error: Invalid JSON in input file: {e}")
            return 1

        # Extract MAPE data and create heatmap
        try:
            mape_data, full_compressor_name = extract_mape_data_for_heatmap(
                report_data, args.compressor
            )
            create_mape_heatmap(mape_data, full_compressor_name, args.output_dir)
            print(
                f"Successfully generated heatmap for compressor: {full_compressor_name}"
            )
            return 0
        except ValueError as e:
            print(f"Error: {e}")
            return 1

    elif args.mode == "heatmap_status_code":
        # Status code heatmap mode - requires input JSON and compressor
        if not args.input:
            parser.error("--input is required for heatmap_status_code mode")
        if not args.compressor:
            parser.error("--compressor is required for heatmap_status_code mode")

        # Load the JSON report
        try:
            with open(args.input[0]) as f:
                report_data = json.load(f)
        except FileNotFoundError:
            print(f"Error: Could not find input file: {args.input}")
            return 1
        except json.JSONDecodeError as e:
            print(f"Error: Invalid JSON in input file: {e}")
            return 1

        # Extract MAPE data for status codes and create heatmap
        try:
            mape_data, full_compressor_name = extract_mape_data_for_status_code_heatmap(
                report_data, args.compressor
            )
            create_status_code_mape_heatmap(
                mape_data, full_compressor_name, args.output_dir
            )
            print(
                f"Successfully generated status code heatmap for compressor: {full_compressor_name}"
            )
            return 0
        except ValueError as e:
            print(f"Error: {e}")
            return 1

    elif args.mode == "operation":
        # Operation fidelity scatter plots mode
        generate_all_operation_plots(args.input[0], output_dir=args.output_dir, cost_config=cost_config)
        print(f"Operation plots generated in {args.output_dir}")
        return 0

    elif args.mode == "graph":
        # Graph fidelity scatter plot mode
        generate_all_graph_plots(args.input[0], output_dir=args.output_dir, cost_config=cost_config)
        print(f"Graph plot generated in {args.output_dir}")
        return 0

    elif args.mode == "error_over_time":
        # Error over time scatter plots mode
        generate_all_error_plots(args.input[0], output_dir=args.output_dir, cost_config=cost_config)
        print(f"Error over time scatter plots generated in {args.output_dir}")
        return 0

    elif args.mode == "rate_over_time":
        # Rate over time scatter plots mode
        generate_all_rate_plots(args.input[0], output_dir=args.output_dir, cost_config=cost_config)
        print(f"Rate over time scatter plots generated in {args.output_dir}")
        return 0

    elif args.mode == "duration_over_time":
        # Duration over time scatter plots mode
        generate_all_duration_plots(
            args.input[0], output_dir=args.output_dir,
            cost_config=cost_config, min_count=args.min_count,
        )
        print(f"Duration over time scatter plots generated in {args.output_dir}")
        return 0

    elif args.mode == "rca":
        # RCA scatter plots mode (TraceRCA and MicroRank)
        generate_all_rca_plots(args.input[0], output_dir=args.output_dir, cost_config=cost_config)
        print(f"RCA scatter plots generated in {args.output_dir}")
        return 0

    elif args.mode == "rca_aggregate":
        # Aggregate RCA scatter plots from multiple reports
        generate_aggregate_rca_plots(
            args.input, output_dir=args.output_dir, cost_config=cost_config, dataset_label=args.label
        )
        print(f"Aggregate RCA scatter plots generated in {args.output_dir}")
        return 0

    elif args.mode == "offset_analysis":
        # Offset analysis plots from TSV output of --analyze-offsets
        generate_all_offset_analysis_plots(args.input[0], output_dir=args.output_dir)
        print(f"Offset analysis plots generated in {args.output_dir}")
        return 0

    # This should never be reached since --mode is required with specific choices
    return 1


if __name__ == "__main__":
    main()
