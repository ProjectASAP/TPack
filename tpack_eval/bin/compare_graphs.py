#!/usr/bin/env python3
"""
CLI tool to compare two graph_results.json files, export diagrams, and calculate graph edit distance.
"""

import argparse
import json
import pathlib
import re

import matplotlib.pyplot as plt
import networkx as nx

# Note: GraphReport itself doesn't need sklearn, but it's required by other
# reports (CountOverTimeReport) that are imported via tpack_eval.reports.__init__.py
from tpack_eval.reports.graph_report import GraphReport


def load_graph_json(json_path: pathlib.Path) -> dict:
    """Load graph_results.json file."""
    with open(json_path) as f:
        return json.load(f)


def detect_head_sampling_ratio(file_path: pathlib.Path) -> float:
    """
    Detect head_sampling ratio from file path.

    head_sampling_N means sample 1 out of N spans.
    So head_sampling_50 means 1/50 sampling rate, and compression ratio is 50.

    Args:
        file_path: Path to graph_results.json file

    Returns:
        Compression ratio (N from head_sampling_N), or 1.0 if not head_sampling
    """
    # Check parent directories for head_sampling pattern
    path_str = str(file_path)
    match = re.search(r"head_sampling_(\d+(?:\.\d+)?)", path_str)
    if match:
        ratio = float(match.group(1))
        return ratio
    return 1.0


def scale_graph_weights(graph_data: dict, scale_factor: float) -> dict:
    """
    Scale edge weights in graph data by a factor.

    Args:
        graph_data: Dictionary with "nodes" and "edges" keys
        scale_factor: Factor to multiply edge weights by

    Returns:
        New graph data with scaled edge weights
    """
    scaled_data = {
        "nodes": graph_data.get("nodes", []),
        "edges": {},
    }

    for edge_str, weight in graph_data.get("edges", {}).items():
        scaled_data["edges"][edge_str] = weight * scale_factor

    return scaled_data


def export_single_graph_diagram(
    G: nx.DiGraph,
    pos: dict,
    ax: plt.Axes,
    title: str,
    node_colors: dict = None,
    edge_colors: dict = None,
):
    """
    Export a single graph diagram on a matplotlib axis.

    Args:
        G: NetworkX graph to visualize
        pos: Node positions (for consistent layout across graphs)
        ax: Matplotlib axis to draw on
        title: Title for the diagram
        node_colors: Dictionary mapping nodes to colors
        edge_colors: Dictionary mapping edges to colors
    """
    # Default colors
    default_node_color = "#87CEEB"  # Sky blue
    default_edge_color = "#666666"  # Gray

    # Determine node colors
    if node_colors:
        node_color_list = [
            node_colors.get(node, default_node_color) for node in G.nodes()
        ]
    else:
        node_color_list = [default_node_color] * G.number_of_nodes()

    # Draw nodes
    nx.draw_networkx_nodes(
        G,
        pos,
        node_color=node_color_list,
        node_size=4000,
        alpha=0.9,
        edgecolors="black",
        linewidths=2.5,
        ax=ax,
    )

    # Draw edges with weights
    edges = list(G.edges())
    if edges:
        weights = [G[u][v].get("weight", 1) for u, v in edges]
        max_weight = max(weights) if weights else 1

        # Determine edge colors
        if edge_colors:
            edge_color_list = [
                edge_colors.get((u, v), default_edge_color) for u, v in edges
            ]
        else:
            edge_color_list = [default_edge_color] * len(edges)

        # Calculate edge widths based on weights
        edge_widths = [2 + (w / max_weight) * 4 for w in weights]

        # Calculate node radius in data coordinates to position arrows properly
        # Node size is in points^2, we need to convert to data coordinates
        node_radius = 35  # Approximation for margin

        nx.draw_networkx_edges(
            G,
            pos,
            edgelist=edges,
            width=edge_widths,
            alpha=0.7,
            edge_color=edge_color_list,
            arrows=True,
            arrowsize=30,
            arrowstyle="-|>",
            ax=ax,
            connectionstyle="arc3,rad=0.15",
            min_source_margin=node_radius,
            min_target_margin=node_radius,
        )

        # Draw edge labels (weights)
        edge_labels = {(u, v): f"{G[u][v].get('weight', 1)}" for u, v in edges}
        nx.draw_networkx_edge_labels(
            G,
            pos,
            edge_labels,
            font_size=10,
            font_weight="bold",
            bbox=dict(
                boxstyle="round,pad=0.4", facecolor="white", alpha=0.8, edgecolor="gray"
            ),
            ax=ax,
        )

    # Draw labels
    nx.draw_networkx_labels(
        G,
        pos,
        font_size=11,
        font_weight="bold",
        font_family="sans-serif",
        ax=ax,
    )

    ax.set_title(title, fontsize=18, fontweight="bold", pad=20)
    ax.axis("off")


def export_comparison_diagram(
    G1: nx.DiGraph,
    G2: nx.DiGraph,
    output_path: pathlib.Path,
    graph1_name: str,
    graph2_name: str,
    timestamp: str,
    distance: float,
    fidelity: float,
):
    """
    Export side-by-side comparison of two graphs with color-coded differences.

    Args:
        G1: First graph (with scaled weights if applicable)
        G2: Second graph (with scaled weights if applicable)
        output_path: Path to save the PNG file
        graph1_name: Name of first graph
        graph2_name: Name of second graph
        timestamp: Timestamp being compared
        distance: Graph edit distance
        fidelity: Graph fidelity score (0-100)
    """
    # Create union graph for consistent layout
    G_union = nx.DiGraph()
    G_union.add_nodes_from(set(G1.nodes()) | set(G2.nodes()))
    G_union.add_edges_from(set(G1.edges()) | set(G2.edges()))

    # Calculate layout once for consistent positioning
    if G_union.number_of_nodes() > 0:
        try:
            pos = nx.spring_layout(G_union, k=2.5, iterations=100, seed=42)
        except:
            pos = nx.shell_layout(G_union)
    else:
        pos = {}

    # Analyze differences
    nodes1_only = set(G1.nodes()) - set(G2.nodes())
    nodes2_only = set(G2.nodes()) - set(G1.nodes())
    common_nodes = set(G1.nodes()) & set(G2.nodes())

    edges1_only = set(G1.edges()) - set(G2.edges())
    edges2_only = set(G2.edges()) - set(G1.edges())
    common_edges = set(G1.edges()) & set(G2.edges())

    # Color schemes
    node_color_g1_only = "#FF6B6B"  # Red
    node_color_g2_only = "#51CF66"  # Green
    node_color_common = "#87CEEB"  # Sky blue

    edge_color_g1_only = "#FF6B6B"  # Red
    edge_color_g2_only = "#51CF66"  # Green
    edge_color_common = "#666666"  # Gray
    edge_color_diff = "#FFA500"  # Orange (different weights)

    # Determine node colors for G1
    node_colors_g1 = {}
    for node in G1.nodes():
        if node in nodes1_only:
            node_colors_g1[node] = node_color_g1_only
        else:
            node_colors_g1[node] = node_color_common

    # Determine node colors for G2
    node_colors_g2 = {}
    for node in G2.nodes():
        if node in nodes2_only:
            node_colors_g2[node] = node_color_g2_only
        else:
            node_colors_g2[node] = node_color_common

    # Determine edge colors for G1
    edge_colors_g1 = {}
    for edge in G1.edges():
        if edge in edges1_only:
            edge_colors_g1[edge] = edge_color_g1_only
        elif edge in common_edges:
            # Check if weights differ
            if G1[edge[0]][edge[1]].get("weight", 0) != G2[edge[0]][edge[1]].get(
                "weight", 0
            ):
                edge_colors_g1[edge] = edge_color_diff
            else:
                edge_colors_g1[edge] = edge_color_common

    # Determine edge colors for G2
    edge_colors_g2 = {}
    for edge in G2.edges():
        if edge in edges2_only:
            edge_colors_g2[edge] = edge_color_g2_only
        elif edge in common_edges:
            # Check if weights differ
            if G1[edge[0]][edge[1]].get("weight", 0) != G2[edge[0]][edge[1]].get(
                "weight", 0
            ):
                edge_colors_g2[edge] = edge_color_diff
            else:
                edge_colors_g2[edge] = edge_color_common

    # Create figure with two subplots
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(24, 10))
    fig.suptitle(
        f"Graph Comparison - Timebucket: {timestamp}",
        fontsize=22,
        fontweight="bold",
        y=0.98,
    )

    # Draw both graphs
    export_single_graph_diagram(
        G1, pos, ax1, f"Graph 1: {graph1_name}", node_colors_g1, edge_colors_g1
    )
    export_single_graph_diagram(
        G2, pos, ax2, f"Graph 2: {graph2_name}", node_colors_g2, edge_colors_g2
    )

    # Add metrics text box (positioned above legend)
    metrics_text = (
        f"Graph Edit Distance: {distance:.2f}\nGraph Fidelity: {fidelity:.2f}%"
    )
    fig.text(
        0.5,
        0.085,
        metrics_text,
        ha="center",
        va="bottom",
        fontsize=16,
        fontweight="bold",
        bbox=dict(
            boxstyle="round,pad=0.8",
            facecolor="lightyellow",
            edgecolor="black",
            linewidth=2,
        ),
    )

    # Add legend (positioned below metrics)
    from matplotlib.patches import Patch

    legend_elements = [
        Patch(facecolor=node_color_g1_only, edgecolor="black", label="Only in Graph 1"),
        Patch(facecolor=node_color_g2_only, edgecolor="black", label="Only in Graph 2"),
        Patch(facecolor=node_color_common, edgecolor="black", label="In Both Graphs"),
        Patch(facecolor=edge_color_diff, edgecolor="black", label="Different Weight"),
    ]
    fig.legend(
        handles=legend_elements,
        loc="lower center",
        bbox_to_anchor=(0.5, 0.01),
        ncol=4,
        fontsize=14,
        frameon=True,
        fancybox=True,
        shadow=True,
    )

    plt.tight_layout(rect=[0, 0.12, 1, 0.96])

    # Save the figure
    plt.savefig(output_path, dpi=150, bbox_inches="tight", facecolor="white")
    plt.close()

    print(f"  Exported comparison diagram: {output_path}")


def compare_graphs(
    graph1_path: pathlib.Path,
    graph2_path: pathlib.Path,
    timestamp: str,
    output_dir: pathlib.Path,
    graph_report: GraphReport,
) -> tuple[float, float]:
    """
    Compare two graphs and export diagrams.

    Args:
        graph1_path: Path to first graph_results.json
        graph2_path: Path to second graph_results.json
        timestamp: Time bucket to compare
        output_dir: Directory to export diagrams
        graph_report: GraphReport instance for calculations

    Returns:
        Tuple of (graph_edit_distance, fidelity_score)
    """
    # Load graphs
    graph1_data = load_graph_json(graph1_path)
    graph2_data = load_graph_json(graph2_path)

    # Get graphs for the specified timestamp
    graphs1 = graph1_data.get("service_graph_by_time", {})
    graphs2 = graph2_data.get("service_graph_by_time", {})

    if timestamp not in graphs1:
        raise ValueError(f"Timebucket {timestamp} not found in {graph1_path}")
    if timestamp not in graphs2:
        raise ValueError(f"Timebucket {timestamp} not found in {graph2_path}")

    graph1_timestamp_data = graphs1[timestamp]
    graph2_timestamp_data = graphs2[timestamp]

    # Detect head_sampling ratios and scale weights if needed
    # head_sampling_N means 1 out of N spans is sampled, so multiply weights by N
    ratio1 = detect_head_sampling_ratio(graph1_path)
    ratio2 = detect_head_sampling_ratio(graph2_path)

    if ratio1 > 1.0:
        graph1_timestamp_data = scale_graph_weights(graph1_timestamp_data, ratio1)
        print(
            f"\n  Detected head_sampling_{ratio1:.0f} in Graph 1, scaling weights by {ratio1:.0f}x"
        )

    if ratio2 > 1.0:
        graph2_timestamp_data = scale_graph_weights(graph2_timestamp_data, ratio2)
        print(
            f"  Detected head_sampling_{ratio2:.0f} in Graph 2, scaling weights by {ratio2:.0f}x"
        )

    # Convert to NetworkX using GraphReport method
    G1 = graph_report.json_to_networkx(graph1_timestamp_data)
    G2 = graph_report.json_to_networkx(graph2_timestamp_data)

    # Calculate distance using GraphReport method
    distance = graph_report.calculate_distance(G1, G2)

    # Calculate total edge weight from both graphs
    num_nodes = G1.number_of_nodes()
    total_edge_weight_g1 = sum(
        data.get("weight", 0) for _, _, data in G1.edges(data=True)
    )
    total_edge_weight_g2 = sum(
        data.get("weight", 0) for _, _, data in G2.edges(data=True)
    )
    total_edge_weight = total_edge_weight_g1 + total_edge_weight_g2

    # Calculate fidelity using GraphReport method
    fidelity = graph_report.calculate_graph_fidelity(
        distance, num_nodes, total_edge_weight
    )

    # Export diagrams
    output_dir.mkdir(parents=True, exist_ok=True)

    graph1_name = graph1_path.parent.parent.name
    graph2_name = graph2_path.parent.parent.name

    # Export side-by-side comparison diagram with scaled weights
    export_comparison_diagram(
        G1,
        G2,
        output_dir / f"comparison_{graph1_name}_vs_{graph2_name}_{timestamp}.png",
        graph1_name,
        graph2_name,
        timestamp,
        distance,
        fidelity,
    )

    return distance, fidelity


def print_graph_statistics(G: nx.DiGraph, name: str):
    """Print graph statistics."""
    print(f"\n{name}:")
    print(f"  Nodes: {G.number_of_nodes()}")
    print(f"  Edges: {G.number_of_edges()}")

    if G.number_of_edges() > 0:
        total_weight = sum(data.get("weight", 0) for _, _, data in G.edges(data=True))
        avg_weight = total_weight / G.number_of_edges()
        print(f"  Total edge weight: {total_weight}")
        print(f"  Average edge weight: {avg_weight:.2f}")

    if G.number_of_nodes() > 0:
        print(f"  Nodes: {', '.join(sorted(G.nodes()))}")


def print_graph_differences(G1: nx.DiGraph, G2: nx.DiGraph):
    """Print detailed differences between two graphs."""
    nodes1 = set(G1.nodes())
    nodes2 = set(G2.nodes())
    edges1 = set(G1.edges())
    edges2 = set(G2.edges())

    nodes1_only = nodes1 - nodes2
    nodes2_only = nodes2 - nodes1
    common_nodes = nodes1 & nodes2

    edges1_only = edges1 - edges2
    edges2_only = edges2 - edges1
    common_edges = edges1 & edges2

    print(f"\n{'=' * 70}")
    print("GRAPH DIFFERENCES")
    print(f"{'=' * 70}")

    # Node differences
    print(f"\nNodes only in Graph 1 ({len(nodes1_only)}):")
    if nodes1_only:
        print(f"  {', '.join(sorted(nodes1_only))}")
    else:
        print("  None")

    print(f"\nNodes only in Graph 2 ({len(nodes2_only)}):")
    if nodes2_only:
        print(f"  {', '.join(sorted(nodes2_only))}")
    else:
        print("  None")

    print(f"\nCommon nodes ({len(common_nodes)}):")
    if common_nodes:
        print(f"  {', '.join(sorted(common_nodes))}")

    # Edge differences
    print(f"\nEdges only in Graph 1 ({len(edges1_only)}):")
    if edges1_only:
        for u, v in sorted(edges1_only):
            weight = G1[u][v].get("weight", 1)
            print(f"  {u} -> {v} (weight: {weight})")
    else:
        print("  None")

    print(f"\nEdges only in Graph 2 ({len(edges2_only)}):")
    if edges2_only:
        for u, v in sorted(edges2_only):
            weight = G2[u][v].get("weight", 1)
            print(f"  {u} -> {v} (weight: {weight})")
    else:
        print("  None")

    # Common edges with different weights
    print("\nCommon edges with different weights:")
    diff_weight_edges = []
    for edge in sorted(common_edges):
        w1 = G1[edge[0]][edge[1]].get("weight", 0)
        w2 = G2[edge[0]][edge[1]].get("weight", 0)
        if w1 != w2:
            diff_weight_edges.append((edge, w1, w2))

    if diff_weight_edges:
        for (u, v), w1, w2 in diff_weight_edges:
            diff = w2 - w1
            sign = "+" if diff > 0 else ""
            print(f"  {u} -> {v}: {w1} vs {w2} ({sign}{diff})")
    else:
        print("  None")

    print(
        f"\nCommon edges with same weights ({len(common_edges) - len(diff_weight_edges)}):"
    )


def main():
    parser = argparse.ArgumentParser(
        description="Compare two graph_results.json files and calculate graph edit distance"
    )
    parser.add_argument(
        "graph1", type=str, help="Path to first graph_results.json file"
    )
    parser.add_argument(
        "graph2", type=str, help="Path to second graph_results.json file"
    )
    parser.add_argument(
        "timestamp", type=str, help="Timestamp (time bucket) to compare"
    )
    parser.add_argument(
        "--output-dir",
        "-o",
        type=str,
        default="graph_comparison_output",
        help="Output directory for graph diagrams (default: graph_comparison_output)",
    )
    parser.add_argument(
        "--list-timestamps",
        "-l",
        action="store_true",
        help="List available timestamps in both graphs and exit",
    )

    args = parser.parse_args()

    graph1_path = pathlib.Path(args.graph1)
    graph2_path = pathlib.Path(args.graph2)
    output_dir = pathlib.Path(args.output_dir)

    # Validate inputs
    if not graph1_path.exists():
        print(f"Error: {graph1_path} does not exist")
        return 1

    if not graph2_path.exists():
        print(f"Error: {graph2_path} does not exist")
        return 1

    # List timestamps if requested
    if args.list_timestamps:
        graph1_data = load_graph_json(graph1_path)
        graph2_data = load_graph_json(graph2_path)

        timestamps1 = set(graph1_data.get("service_graph_by_time", {}).keys())
        timestamps2 = set(graph2_data.get("service_graph_by_time", {}).keys())

        print(f"\nTimestamps in {graph1_path}:")
        for ts in sorted(timestamps1):
            print(f"  {ts}")

        print(f"\nTimestamps in {graph2_path}:")
        for ts in sorted(timestamps2):
            print(f"  {ts}")

        common_timestamps = timestamps1 & timestamps2
        print(f"\nCommon timestamps: {len(common_timestamps)}")
        if common_timestamps:
            print("  " + ", ".join(sorted(common_timestamps)))

        return 0

    # Create GraphReport instance for using its methods
    graph_report = GraphReport()

    # Perform comparison
    print(f"\n{'=' * 70}")
    print("Graph Comparison Tool")
    print(f"{'=' * 70}")
    print(f"\nGraph 1: {graph1_path}")
    print(f"Graph 2: {graph2_path}")
    print(f"Timebucket: {args.timestamp}")
    print(f"Output directory: {output_dir}")

    try:
        # Load and display graph statistics
        graph1_data = load_graph_json(graph1_path)
        graph2_data = load_graph_json(graph2_path)

        graphs1 = graph1_data.get("service_graph_by_time", {})
        graphs2 = graph2_data.get("service_graph_by_time", {})

        # Use GraphReport's json_to_networkx method
        G1 = graph_report.json_to_networkx(graphs1[args.timestamp])
        G2 = graph_report.json_to_networkx(graphs2[args.timestamp])

        print_graph_statistics(G1, "Graph 1 Statistics")
        print_graph_statistics(G2, "Graph 2 Statistics")

        # Print detailed differences
        print_graph_differences(G1, G2)

        # Compare and export
        print(f"\n{'=' * 70}")
        print("Comparing graphs and exporting diagrams...")
        print(f"{'=' * 70}")

        distance, fidelity = compare_graphs(
            graph1_path, graph2_path, args.timestamp, output_dir, graph_report
        )

        # Display results
        print(f"\n{'=' * 70}")
        print("RESULTS")
        print(f"{'=' * 70}")
        print(f"\nGraph Edit Distance: {distance:.2f}")
        print(f"Graph Fidelity Score: {fidelity:.2f}%")
        print(f"\nDiagrams exported to: {output_dir.absolute()}")
        print(f"{'=' * 70}\n")

        return 0

    except Exception as e:
        print(f"\nError: {e}")
        import traceback

        traceback.print_exc()
        return 1


if __name__ == "__main__":
    exit(main())
