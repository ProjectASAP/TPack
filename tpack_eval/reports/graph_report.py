import re

import networkx as nx


class GraphReport:
    """Graph comparison utilities for calculating graph edit distance and fidelity."""

    def calculate_distance(self, G1: nx.DiGraph, G2: nx.DiGraph) -> float:
        """Calculate graph edit distance with custom cost functions."""
        distance_generator = nx.optimize_graph_edit_distance(
            G1,
            G2,
            node_subst_cost=lambda n1, n2: 0 if n1 == n2 else float("inf"),
            node_del_cost=lambda n: 1,
            node_ins_cost=lambda n: 1,
            edge_subst_cost=lambda e1, e2: abs(
                e1.get("weight", 0) - e2.get("weight", 0)
            ),
            edge_del_cost=lambda e: e.get("weight", 1),
            edge_ins_cost=lambda e: e.get("weight", 1),
        )
        return next(distance_generator)

    def calculate_graph_fidelity(
        self, distance: float, num_nodes: int, total_edge_weight: float
    ) -> float:
        """Calculate fidelity score (0-100) from graph edit distance."""
        reference_size = num_nodes + total_edge_weight
        if reference_size == 0:
            return 100.0
        return max(0.0, 100.0 - (distance / reference_size) * 100)

    def json_to_networkx(self, graph_data: dict) -> nx.DiGraph:
        """Convert JSON graph representation to NetworkX DiGraph."""
        G = nx.DiGraph()
        G.add_nodes_from(graph_data.get("nodes", []))
        for edge_str, weight in graph_data.get("edges", {}).items():
            if "->" in edge_str:
                parent, child = edge_str.split("->")
                G.add_edge(parent, child, weight=weight)
        return G

    def calculate_compression_ratio(self, compressor: str) -> float:
        """Extract compression ratio N from head_sampling_N compressor name."""
        match = re.search(r"head_sampling_(\d+(?:\.\d+)?)", compressor)
        if match:
            return float(match.group(1))
        return 1.0

    def scale_edge_weights(self, graph_data: dict, scale_factor: float) -> dict:
        """Scale edge weights by a factor (for head_sampling compressors)."""
        return {
            "nodes": graph_data.get("nodes", []),
            "edges": {k: v * scale_factor for k, v in graph_data.get("edges", {}).items()},
        }
