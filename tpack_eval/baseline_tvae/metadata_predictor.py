"""Statistical metadata predictor for high-cardinality columns.

For each span type (service_name, operation_name), stores the empirical
distribution of each high-cardinality column and samples from it during
reconstruction.  This mirrors TPack's statistical metadata predictor so
the tVAE strawman comparison is fair.
"""

from __future__ import annotations

import numpy as np

from tpack_eval.baseline_tvae.flatten import FlatDataset


class StatisticalMetadataPredictor:
    """Per-span-type categorical sampler for high-cardinality columns."""

    def __init__(self) -> None:
        # {col: {(svc, op): (values, cumprobs)}}
        self._distributions: dict[str, dict[tuple[str, str], tuple[list[str], np.ndarray]]] = {}
        # {col: (values, cumprobs)} — fallback for unseen span types
        self._global: dict[str, tuple[list[str], np.ndarray]] = {}
        self.columns: list[str] = []

    def fit(self, dataset: FlatDataset) -> None:
        """Learn per-(service, operation) distributions for skipped columns."""
        if not dataset.skipped_raw:
            return

        self.columns = sorted(dataset.skipped_raw.keys())

        # Decode service_name and operation_name to strings for grouping
        svc_vocab = dataset.vocabs["service_name"]
        op_vocab = dataset.vocabs["operation_name"]
        svc_indices = dataset.categoricals["service_name"]
        op_indices = dataset.categoricals["operation_name"]
        n = dataset.n_spans

        svc_strs = [svc_vocab[min(int(idx), len(svc_vocab) - 1)] for idx in svc_indices]
        op_strs = [op_vocab[min(int(idx), len(op_vocab) - 1)] for idx in op_indices]

        for col in self.columns:
            raw = dataset.skipped_raw[col]
            # Group values by span type
            groups: dict[tuple[str, str], list[str]] = {}
            global_vals: list[str] = []
            for i in range(n):
                key = (svc_strs[i], op_strs[i])
                if key not in groups:
                    groups[key] = []
                groups[key].append(raw[i])
                global_vals.append(raw[i])

            # Build distributions
            col_dists: dict[tuple[str, str], tuple[list[str], np.ndarray]] = {}
            for key, vals in groups.items():
                col_dists[key] = _build_distribution(vals)
            self._distributions[col] = col_dists
            self._global[col] = _build_distribution(global_vals)

        print(f"  Metadata predictor: {len(self.columns)} columns, "
              f"{sum(len(d) for d in self._distributions.values())} span types")

    def sample(
        self,
        service_names: list[str],
        operation_names: list[str],
        rng: np.random.Generator,
    ) -> dict[str, list[str]]:
        """Sample metadata values for each span based on its type."""
        n = len(service_names)
        result: dict[str, list[str]] = {}

        for col in self.columns:
            col_dists = self._distributions[col]
            global_dist = self._global[col]
            values: list[str] = []

            for i in range(n):
                key = (service_names[i], operation_names[i])
                dist = col_dists.get(key, global_dist)
                vals, cumprobs = dist
                r = rng.random()
                idx = np.searchsorted(cumprobs, r)
                values.append(vals[min(idx, len(vals) - 1)])

            result[col] = values

        return result


def _build_distribution(values: list[str]) -> tuple[list[str], np.ndarray]:
    """Build (sorted_values, cumulative_probabilities) from a list of strings."""
    counts: dict[str, int] = {}
    for v in values:
        counts[v] = counts.get(v, 0) + 1
    sorted_vals = sorted(counts.keys())
    probs = np.array([counts[v] for v in sorted_vals], dtype=np.float64)
    probs /= probs.sum()
    return sorted_vals, np.cumsum(probs)
