"""Shared utilities for scatter plot visualization."""

import matplotlib.pyplot as plt
import numpy as np


# Experiment name to display name mapping
EXPERIMENT_DISPLAY_NAMES = {
    "tpack": "TPack",
    "ratioregression": "Ratio+Reg",
    "ratiopercent": "Ratio+Pct",
    "absregression": "Abs+Reg",
    "abspercent": "Abs+Pct",
    "simple": "TPack-Simple",
    "vae": "TPack-VAE",
}

# Color scheme for TPack experiments
EXPERIMENT_COLORS = {
    "tpack": "red",
    "ratioregression": "red",
    "ratiopercent": "darkorange",
    "absregression": "blue",
    "abspercent": "darkcyan",
    "simple": "gray",
    "vae": "green",
}
EXPERIMENT_MARKERS = {
    "tpack": "*",
    "ratioregression": "o",
    "ratiopercent": "s",
    "absregression": "^",
    "abspercent": "D",
    "simple": "v",
    "vae": "X",
}

# Head sampling style (consistent color for all rates)
HEAD_SAMPLING_MARKER = "D"
HEAD_SAMPLING_COLOR = "purple"


def setup_plot_style():
    """Setup plot figure and style."""
    plt.figure(figsize=(14, 6))
    plt.style.use("default")
    plt.rcParams["font.size"] = 14
    plt.rcParams["axes.labelsize"] = 16
    plt.rcParams["axes.titlesize"] = 20
    plt.rcParams["xtick.labelsize"] = 14
    plt.rcParams["ytick.labelsize"] = 14


def get_display_name(experiment_name: str) -> str:
    """Get display name for an experiment, using mapping or the raw name."""
    return EXPERIMENT_DISPLAY_NAMES.get(experiment_name, experiment_name)


def group_experiments_by_name(data_points, metric):
    """
    Group experiments by experiment name for TPack and by sampling rate for head sampling.

    Args:
        data_points: List of data points with 'compressor', 'total_cost', and metric field
        metric: Name of the metric field to extract (e.g., 'mape_fidelity')

    Returns:
        Tuple of (experiment_groups, head_sampling_groups)
        - experiment_groups: dict {experiment_name: [(cost, fidelity, compressor), ...]}
        - head_sampling_groups: dict {sampling_rate: [(cost, fidelity, compressor), ...]}
    """
    experiment_groups = {}
    head_sampling_groups = {}

    for point in data_points:
        compressor = point["compressor"]
        cost = point["total_cost"]
        fidelity = point[metric]

        parts = compressor.split("_")

        # Format: {prefix}_gent_{experiment_name}_{iteration}
        # or: {prefix}_head_{sampling_rate}_{iteration}
        if "tpack" in parts:
            tpack_idx = parts.index("tpack")
            if tpack_idx + 1 < len(parts):
                next_part = parts[tpack_idx + 1]
                # If next part is a number, it's the iteration (format: tpack_{iter})
                # Otherwise it's the experiment name (format: tpack_{name}_{iter})
                if next_part.isdigit():
                    experiment_name = "tpack"
                elif tpack_idx + 2 < len(parts):
                    experiment_name = next_part
                else:
                    continue
                if experiment_name not in EXPERIMENT_DISPLAY_NAMES:
                    continue
                if experiment_name not in experiment_groups:
                    experiment_groups[experiment_name] = []
                experiment_groups[experiment_name].append((cost, fidelity, compressor))
        elif "head" in parts:
            head_idx = parts.index("head")
            if head_idx + 2 < len(parts):
                sampling_rate = parts[head_idx + 1]
                if sampling_rate not in head_sampling_groups:
                    head_sampling_groups[sampling_rate] = []
                head_sampling_groups[sampling_rate].append((cost, fidelity, compressor))

    return experiment_groups, head_sampling_groups


def plot_experiment_groups(experiment_groups):
    """
    Plot TPack experiment groups with mean ± std error bars.

    Args:
        experiment_groups: dict {experiment_name: [(cost, fidelity, compressor), ...]}
    """
    for experiment_name in sorted(experiment_groups.keys()):
        group_data = experiment_groups[experiment_name]
        group_costs = [d[0] for d in group_data]
        group_fidelities = [d[1] for d in group_data]

        mean_cost = np.mean(group_costs)
        mean_fidelity = np.mean(group_fidelities)
        std_cost = np.std(group_costs)
        std_fidelity = np.std(group_fidelities)

        color = EXPERIMENT_COLORS.get(experiment_name, "black")
        marker = EXPERIMENT_MARKERS.get(experiment_name, "o")
        display_name = get_display_name(experiment_name)

        # Plot error bars (mean ± std) — x=fidelity, y=cost
        plt.errorbar(
            x=mean_fidelity,
            y=mean_cost,
            xerr=std_fidelity,
            yerr=std_cost,
            marker=marker,
            markersize=10,
            label=f"{display_name} (mean ± std)",
            color=color,
            alpha=0.8,
            capsize=6,
            capthick=2,
            linewidth=2,
        )

        # Plot individual points
        plt.scatter(
            x=group_fidelities,
            y=group_costs,
            marker=marker,
            s=60,
            color=color,
            alpha=0.4,
            edgecolors="black",
            linewidth=1,
        )

        # Annotate mean
        plt.annotate(
            display_name,
            (mean_fidelity, mean_cost),
            xytext=(10, 10),
            textcoords="offset points",
            fontsize=12,
            fontweight="bold",
            bbox={
                "boxstyle": "round,pad=0.2",
                "facecolor": "white",
                "alpha": 0.7,
            },
            arrowprops={"arrowstyle": "->", "color": "gray", "alpha": 0.5},
        )


def plot_head_sampling_points(head_sampling_groups):
    """
    Plot head sampling groups with mean ± std error bars.

    Args:
        head_sampling_groups: dict {sampling_rate: [(cost, fidelity, compressor), ...]}
    """
    if not head_sampling_groups:
        return

    # Sort by sampling rate (as int for proper ordering)
    sorted_rates = sorted(head_sampling_groups.keys(), key=lambda x: int(x))
    first_rate = True

    for rate in sorted_rates:
        group_data = head_sampling_groups[rate]
        group_costs = [d[0] for d in group_data]
        group_fidelities = [d[1] for d in group_data]

        mean_cost = np.mean(group_costs)
        mean_fidelity = np.mean(group_fidelities)
        std_cost = np.std(group_costs)
        std_fidelity = np.std(group_fidelities)

        display_name = f"1:{rate}"

        # Plot error bars (mean ± std) — x=fidelity, y=cost
        # Only add legend label for the first rate to avoid duplicate "Head Sampling" entries
        plt.errorbar(
            x=mean_fidelity,
            y=mean_cost,
            xerr=std_fidelity,
            yerr=std_cost,
            marker=HEAD_SAMPLING_MARKER,
            markersize=10,
            label="Head Sampling (mean ± std)" if first_rate else None,
            color=HEAD_SAMPLING_COLOR,
            alpha=0.8,
            capsize=6,
            capthick=2,
            linewidth=2,
        )

        # Plot individual points
        plt.scatter(
            x=group_fidelities,
            y=group_costs,
            marker=HEAD_SAMPLING_MARKER,
            s=60,
            color=HEAD_SAMPLING_COLOR,
            alpha=0.4,
            edgecolors="black",
            linewidth=1,
        )

        # Annotate mean
        plt.annotate(
            display_name,
            (mean_fidelity, mean_cost),
            xytext=(10, 10),
            textcoords="offset points",
            fontsize=12,
            fontweight="bold",
            bbox={
                "boxstyle": "round,pad=0.2",
                "facecolor": "white",
                "alpha": 0.7,
            },
            arrowprops={"arrowstyle": "->", "color": "gray", "alpha": 0.5},
        )

        first_rate = False


def format_plot_axes(title, y_label):
    """
    Format plot axes, title, labels, and grid.

    Args:
        title: Full title for the plot (e.g., "Rate Over Time: MAPE vs Cost - 0 filters")
        y_label: Y-axis label (e.g., "MAPE Fidelity (%)", "TraceRCA Avg@5 Fidelity (%)")
    """
    plt.title(
        title,
        fontsize=22,
        fontweight="bold",
        pad=20,
    )
    plt.xlabel(f"{y_label} (→)", fontsize=18, fontweight="bold")
    plt.ylabel("Total Cost ($) (←)", fontsize=18, fontweight="bold")
    plt.yscale("log")
    plt.xlim(0, 100)

    # Legend
    plt.legend(
        loc="best",
        frameon=True,
        fancybox=True,
        shadow=True,
        fontsize=14,
    )

    # Grid
    plt.grid(True, which="both", linestyle="--", linewidth=0.5, alpha=0.7)
    plt.grid(True, which="minor", linestyle=":", linewidth=0.3, alpha=0.5)


def build_filter_title(base_title, metric, filter_level):
    """
    Build title for rate/duration plots with filter levels.

    Args:
        base_title: Base title (e.g., "Rate Over Time", "Duration Over Time")
        metric: Metric name ('mape_fidelity' or 'cosine_fidelity')
        filter_level: Filter level (0, 1, or 2)

    Returns:
        Full formatted title string
    """
    metric_name = "MAPE" if metric == "mape_fidelity" else "Cosine Similarity"
    if filter_level == 1:
        filter_desc = "1 filter"
    else:
        filter_desc = f"{filter_level} filters"
    return f"{base_title}: {metric_name} vs Cost - {filter_desc}"
