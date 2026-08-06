"""Read flattened span CSV (produced by Go tpack-eval --flatten-csv) into a FlatDataset."""

import csv
import os
import time
from dataclasses import dataclass, field

import numpy as np


@dataclass
class FlatDataset:
    """Flat tabular representation of spans.

    All columns are stored uniformly in dicts:
      continuous: name → float64 array (fed to VAE as VGM-encoded values)
      categoricals: name → int64 index array (fed to VAE as one-hot)
      vocabs: name → vocabulary list (index 0 = "<NONE>" for nullable columns)
      minute_bucket: int64 array (grouping only, not fed to VAE)
    """

    continuous: dict[str, np.ndarray] = field(default_factory=dict)
    categoricals: dict[str, np.ndarray] = field(default_factory=dict)
    vocabs: dict[str, list[str]] = field(default_factory=dict)
    minute_bucket: np.ndarray = field(default_factory=lambda: np.array([], dtype=np.int64))
    skipped_raw: dict[str, list[str]] = field(default_factory=dict)

    @property
    def n_spans(self) -> int:
        return len(self.minute_bucket)


def _build_vocab(values: list[str], nullable: bool = True) -> tuple[list[str], dict[str, int]]:
    """Build vocabulary and index mapping from string values.

    If nullable, index 0 is "<NONE>" (for empty strings).
    """
    unique = sorted(set(v for v in values if v))
    if nullable:
        vocab = ["<NONE>"] + unique
    else:
        vocab = unique
    return vocab, {v: i for i, v in enumerate(vocab)}


def read_csv(csv_path: str) -> FlatDataset:
    """Read a flat CSV produced by `tpack-eval --flatten-csv`.

    Core columns (always present):
        trace_id, span_id, parent_span_id, service_name, operation_name,
        span_kind, status_code, start_time_us, duration_us, depth,
        parent_service_name, parent_operation_name, num_siblings, minute_bucket

    Any additional columns are treated as extra categoricals.
    """
    t0 = time.time()
    print(f"Reading CSV: {csv_path} ({os.path.getsize(csv_path) / 1e6:.0f} MB)")

    # Columns only used for computing derived values (not stored directly)
    meta_columns = {"trace_id", "span_id", "parent_span_id", "start_time_us"}
    # Columns stored as continuous
    continuous_columns = {"duration_us", "depth", "num_siblings"}
    # Columns stored as categorical (with their nullable flag)
    categorical_spec = {
        "service_name": True,      # <NONE> for parent columns
        "operation_name": True,
        "span_kind": False,
        "status_code": False,
        "parent_service_name": True,
        "parent_operation_name": True,
    }
    # Grouping column
    grouping_columns = {"minute_bucket"}

    # Collect raw string lists per column
    raw: dict[str, list] = {}

    with open(csv_path, newline="") as f:
        reader = csv.DictReader(f)
        all_columns = list(reader.fieldnames or [])
        for col in all_columns:
            raw[col] = []

        for row in reader:
            for col in all_columns:
                raw[col].append(row.get(col, ""))

    n = len(raw.get("trace_id", []))
    print(f"  Read {n} rows in {time.time() - t0:.1f}s")

    # Compute start_offset_us and is_root
    t0 = time.time()
    trace_min_us: dict[str, int] = {}
    start_us_list = [int(v) for v in raw["start_time_us"]]
    for i in range(n):
        tid = raw["trace_id"][i]
        us = start_us_list[i]
        if tid not in trace_min_us or us < trace_min_us[tid]:
            trace_min_us[tid] = us

    start_offset = np.empty(n, dtype=np.float64)
    is_root_vals = []
    for i in range(n):
        start_offset[i] = start_us_list[i] - trace_min_us[raw["trace_id"][i]]
        is_root_vals.append("1" if not raw["parent_span_id"][i] else "0")
    print(f"  Computed offsets in {time.time() - t0:.1f}s")

    # Build continuous columns
    t0 = time.time()
    continuous = {
        "duration_us": np.array([float(v) for v in raw["duration_us"]], dtype=np.float64),
        "start_offset_us": start_offset,
        "depth": np.array([float(v) for v in raw["depth"]], dtype=np.float64),
        "num_siblings": np.array([float(v) for v in raw["num_siblings"]], dtype=np.float64),
    }

    # Build categorical columns — share vocabs for parent columns
    vocabs: dict[str, list[str]] = {}
    idx_maps: dict[str, dict[str, int]] = {}

    # Build vocabs for core categoricals
    for col, nullable in categorical_spec.items():
        # parent columns share vocab with their base column
        if col.startswith("parent_"):
            base = col.removeprefix("parent_")
            if base not in vocabs:
                vocabs[base], idx_maps[base] = _build_vocab(raw[base], nullable=True)
            vocabs[col] = vocabs[base]
            idx_maps[col] = idx_maps[base]
        else:
            vocabs[col], idx_maps[col] = _build_vocab(raw[col], nullable=nullable)

    # is_root as categorical
    vocabs["is_root"], idx_maps["is_root"] = _build_vocab(is_root_vals, nullable=False)

    # Extra columns: anything not in known sets (skip high-cardinality columns from VAE)
    MAX_VOCAB = 50  # skip columns with more unique values (e.g. net.peer.port has 3432)
    known = meta_columns | continuous_columns | set(categorical_spec) | grouping_columns
    extra_cols_used = []
    skipped_raw: dict[str, list[str]] = {}
    for col in all_columns:
        if col not in known:
            vocab, idx_map = _build_vocab(raw[col], nullable=True)
            if len(vocab) > MAX_VOCAB:
                print(f"  skip high-cardinality column: {col} ({len(vocab)} values > {MAX_VOCAB})")
                skipped_raw[col] = raw[col]
                continue
            vocabs[col] = vocab
            idx_maps[col] = idx_map
            extra_cols_used.append(col)

    # Encode all categoricals
    categoricals: dict[str, np.ndarray] = {}
    for col in list(categorical_spec) + ["is_root"]:
        src = raw.get(col, is_root_vals if col == "is_root" else [])
        categoricals[col] = np.array([idx_maps[col].get(v, 0) for v in src], dtype=np.int64)

    for col in extra_cols_used:
        categoricals[col] = np.array([idx_maps[col].get(v, 0) for v in raw[col]], dtype=np.int64)

    print(f"  Encoded categoricals in {time.time() - t0:.1f}s")

    ds = FlatDataset(
        continuous=continuous,
        categoricals=categoricals,
        vocabs=vocabs,
        minute_bucket=np.array([int(v) for v in raw["minute_bucket"]], dtype=np.int64),
        skipped_raw=skipped_raw,
    )

    print(f"Vocabularies: {', '.join(f'{k}({len(v)})' for k, v in sorted(vocabs.items()))}")
    print(f"Minute buckets: {len(np.unique(ds.minute_bucket))}")

    return ds
