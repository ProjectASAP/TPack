"""Reconstruct traces from generated flat rows and write as OTLP JSON.

Simple depth-based reconstruction:
  - depth-0 spans = roots, each starts a trace
  - depth-d spans round-robin onto depth-(d-1) spans as parents
  - No string matching — O(n) total
"""

import json
import os
import uuid
from collections import defaultdict

import numpy as np

from tpack_eval.baseline_tvae.flatten import FlatDataset
from tpack_eval.baseline_tvae.metadata_predictor import StatisticalMetadataPredictor
from tpack_eval.baseline_tvae.vae_model import VGMTransformer

_KIND_TO_INT = {
    "UNSPECIFIED": 0, "INTERNAL": 1, "SERVER": 2, "CLIENT": 3,
    "PRODUCER": 4, "CONSUMER": 5,
    "0": 0, "1": 1, "2": 2, "3": 3, "4": 4, "5": 5,
}

# Columns handled by dedicated OTLP fields (not written as span attributes)
_STRUCTURAL = {
    "service_name", "operation_name", "span_kind", "status_code",
    "parent_service_name", "parent_operation_name", "is_root",
    "duration_us", "start_offset_us", "depth", "num_siblings",
}


def _parse_kind(val: str) -> int:
    return _KIND_TO_INT.get(val, 0)


def _parse_status(val: str) -> int:
    try:
        return int(val)
    except ValueError:
        return 0


def _random_hex(n_bytes: int) -> str:
    return uuid.uuid4().hex[:n_bytes * 2]


def _vocab_lookup(decoded_indices: np.ndarray, vocab: list[str]) -> list[str]:
    """Convert decoded int indices to string values using vocab."""
    max_idx = len(vocab) - 1
    return [vocab[min(int(idx), max_idx)] for idx in decoded_indices]


def reconstruct_and_write(
    generated: np.ndarray,
    transformer: VGMTransformer,
    dataset: FlatDataset,
    output_dir: str,
    bucket_id: int = 0,
    metadata_predictor: StatisticalMetadataPredictor | None = None,
):
    """Reconstruct traces using simple depth-based round-robin and write OTLP JSON."""
    os.makedirs(output_dir, exist_ok=True)

    decoded = transformer.inverse_transform(generated)
    n = generated.shape[0]

    # Decode structural columns
    svc = _vocab_lookup(decoded["service_name"], dataset.vocabs["service_name"])
    op = _vocab_lookup(decoded["operation_name"], dataset.vocabs["operation_name"])
    kind = [_parse_kind(v) for v in _vocab_lookup(decoded["span_kind"], dataset.vocabs["span_kind"])]
    status = [_parse_status(v) for v in _vocab_lookup(decoded["status_code"], dataset.vocabs["status_code"])]
    dur_us = np.maximum(decoded["duration_us"], 0)
    offset_us = np.maximum(decoded["start_offset_us"], 0)
    depth = np.maximum(np.round(decoded["depth"]).astype(int), 0)

    bucket_start_us = bucket_id * 60_000_000
    bucket_start_ns = bucket_start_us * 1000

    # Decode all non-structural categoricals as OTLP attributes
    attr_columns: dict[str, list[str]] = {}
    for col, vocab in dataset.vocabs.items():
        if col not in _STRUCTURAL and col in decoded:
            attr_columns[col] = _vocab_lookup(decoded[col], vocab)

    # Sample high-cardinality metadata columns
    if metadata_predictor and metadata_predictor.columns:
        rng = np.random.default_rng(bucket_id)
        meta_cols = metadata_predictor.sample(svc, op, rng)
        attr_columns.update(meta_cols)

    # Group by depth for tree reconstruction
    by_depth: dict[int, list[int]] = defaultdict(list)
    for i in range(n):
        by_depth[depth[i]].append(i)

    max_depth = max(by_depth.keys()) if by_depth else 0

    span_ids = [_random_hex(8) for _ in range(n)]
    parent_span_ids = [""] * n
    trace_ids = [""] * n

    # depth 0 = roots
    for idx in by_depth.get(0, []):
        trace_ids[idx] = _random_hex(16)

    # depth d: round-robin onto depth-(d-1) spans
    for d in range(1, max_depth + 1):
        parents = by_depth.get(d - 1, [])
        children = by_depth.get(d, [])
        if not parents:
            for idx in children:
                trace_ids[idx] = _random_hex(16)
            continue
        for i, idx in enumerate(children):
            parent_idx = parents[i % len(parents)]
            parent_span_ids[idx] = span_ids[parent_idx]
            trace_ids[idx] = trace_ids[parent_idx]

    for i in range(n):
        if not trace_ids[i]:
            trace_ids[i] = _random_hex(16)

    # Build OTLP JSON grouped by service
    by_service: dict[str, list[dict]] = defaultdict(list)
    for i in range(n):
        start_ns = bucket_start_ns + int(offset_us[i] * 1000)
        end_ns = start_ns + int(dur_us[i] * 1000)

        attrs = []
        for col, values in attr_columns.items():
            v = values[i]
            if v and v != "<NONE>":
                attrs.append({"key": col, "value": {"stringValue": str(v)}})

        span_obj: dict = {
            "traceId": trace_ids[i],
            "spanId": span_ids[i],
            "name": op[i],
            "kind": int(kind[i]),
            "startTimeUnixNano": str(start_ns),
            "endTimeUnixNano": str(end_ns),
            "attributes": attrs,
            "status": {} if status[i] == 0 else {"code": int(status[i])},
        }
        if parent_span_ids[i]:
            span_obj["parentSpanId"] = parent_span_ids[i]
        by_service[svc[i]].append(span_obj)

    resource_spans = []
    for service, spans in sorted(by_service.items()):
        resource_spans.append({
            "resource": {
                "attributes": [
                    {"key": "service.name", "value": {"stringValue": service}},
                ]
            },
            "scopeSpans": [{"scope": {"name": "strawman_vae"}, "spans": spans}],
        })

    output_path = os.path.join(output_dir, f"chunk_{bucket_id:020d}_0000.json")
    with open(output_path, "w") as f:
        json.dump({"resourceSpans": resource_spans}, f, separators=(",", ":"))

    return len(resource_spans)
