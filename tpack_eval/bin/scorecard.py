"""Print per-approach fidelity stats and cost breakdown from one or more report.json files."""

import argparse
import glob as globmod
import json
import pathlib
import statistics
import sys

from tpack_eval.plotting.data import CostConfig, ReportParser


# ── Fidelity score extraction ─────────────────────────────────────────────

def collect_fidelity_scores_by_category(report_data: dict, compressor_key: str) -> dict[str, list[float]]:
    """Collect fidelity scores grouped by category for a compressor key.

    Categories:
        - duration  → duration_over_time p50/p90/p99 mape_fidelity per group
        - rate      → rate_over_time mape_fidelity per group
        - error     → error_over_time mape_fidelity per group
        - graph / graph_binary → fidelity per time_* bucket
        - trace_rca / micro_rank → ac@5 (0/1 → 0/100)
    """
    reports = report_data.get("reports", {})
    scores: dict[str, list[float]] = {
        "duration": [],
        "rate": [],
        "error": [],
        "graph": [],
        "graph_binary": [],
        "trace_rca": [],
        "micro_rank": [],
    }

    for section in ("duration_over_time_p50", "duration_over_time_p90", "duration_over_time_p99"):
        section_data = reports.get(section, {})
        if compressor_key not in section_data:
            continue
        for group_data in section_data[compressor_key].values():
            if isinstance(group_data, dict) and "mape_fidelity" in group_data:
                scores["duration"].append(float(group_data["mape_fidelity"]))

    for section, cat in (("rate_over_time", "rate"), ("error_over_time", "error")):
        section_data = reports.get(section, {})
        if compressor_key not in section_data:
            continue
        for group_data in section_data[compressor_key].values():
            if isinstance(group_data, dict) and "mape_fidelity" in group_data:
                scores[cat].append(float(group_data["mape_fidelity"]))

    for section, cat in (("graph", "graph"), ("graph_binary", "graph_binary")):
        section_data = reports.get(section, {}).get(compressor_key, {})
        for bucket_key, bucket_data in section_data.items():
            if bucket_key.startswith("time_") and isinstance(bucket_data, dict) and "fidelity" in bucket_data:
                scores[cat].append(float(bucket_data["fidelity"]))

    for rca_key in ("trace_rca", "micro_rank"):
        rca_data = reports.get(rca_key, {}).get(compressor_key, {})
        if isinstance(rca_data, dict) and "ac5" in rca_data:
            scores[rca_key].append(float(rca_data["ac5"]["avg"]) * 100)

    return scores


# ── Key resolution ────────────────────────────────────────────────────────

def find_all_compressor_keys(report_data: dict, prefix: str) -> list[str]:
    """Find all compressor keys in the report matching a prefix (e.g., all seeds)."""
    reports = report_data.get("reports", {})
    found = set()
    prefix_parts = prefix.split("_")
    for section_data in reports.values():
        if not isinstance(section_data, dict):
            continue
        for key in section_data:
            key_parts = key.split("_")
            for i in range(len(key_parts) - len(prefix_parts) + 1):
                if key_parts[i : i + len(prefix_parts)] == prefix_parts:
                    found.add(key)
                    break
    return sorted(found)


# ── Display ───────────────────────────────────────────────────────────────

DISPLAY_NAMES = {
    "head": "1:{rate} Head",
    "tpack_simple": "TPack-Simple",
    "tpack_vae": "TPack-VAE",
    "tail": "Tail",
    "hindsight": "Hindsight",
}


def approach_display_name(prefix: str) -> str:
    for pattern, name in DISPLAY_NAMES.items():
        if prefix.startswith(pattern):
            return name
    return prefix


# ── Main ──────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(
        description="Print per-approach fidelity stats and cost breakdown from report.json files"
    )
    parser.add_argument("--input", required=True, nargs="+", help="Path(s) to report.json file(s), or glob patterns")
    parser.add_argument(
        "--approaches",
        nargs="+",
        default=["tpack_simple", "tpack_vae"],
        help="Approach prefixes to evaluate (default: tpack_simple tpack_vae)",
    )
    args = parser.parse_args()

    # Resolve input paths (support globs)
    report_paths = []
    for pattern in args.input:
        expanded = sorted(globmod.glob(pattern, recursive=True))
        if expanded:
            report_paths.extend(expanded)
        else:
            report_paths.append(pattern)  # let it fail with a clear error

    # Load all reports
    all_report_data = []
    all_experiments = []
    for rp_str in report_paths:
        rp_path = pathlib.Path(rp_str)
        if not rp_path.exists():
            print(f"Error: {rp_path} not found", file=sys.stderr)
            sys.exit(1)
        with open(rp_path) as f:
            all_report_data.append(json.load(f))
        rp = ReportParser(CostConfig())
        all_experiments.extend(rp.parse_report(str(rp_path)))

    if len(report_paths) > 1:
        print(f"Aggregating {len(report_paths)} reports")

    for approach in args.approaches:
        all_seed_keys: list[str] = []
        for rd in all_report_data:
            all_seed_keys.extend(find_all_compressor_keys(rd, approach))
        seed_keys = sorted(set(all_seed_keys))

        agg_scores_by_cat: dict[str, list[float]] = {
            "duration": [], "rate": [], "error": [], "graph": [], "graph_binary": [],
            "trace_rca": [], "micro_rank": [],
        }
        seed_means_by_cat: dict[str, list[float]] = {k: [] for k in agg_scores_by_cat}
        approach_key = None
        for sk in seed_keys:
            seed_scores_by_cat: dict[str, list[float]] = {k: [] for k in agg_scores_by_cat}
            for rd in all_report_data:
                by_cat = collect_fidelity_scores_by_category(rd, sk)
                for cat, vals in by_cat.items():
                    seed_scores_by_cat[cat].extend(vals)
                    agg_scores_by_cat[cat].extend(vals)
            approach_key = sk
            for cat, vals in seed_scores_by_cat.items():
                if vals:
                    seed_means_by_cat[cat].append(sum(vals) / len(vals))

        if approach_key is None:
            print(f"Warning: approach '{approach}' not found in any report", file=sys.stderr)
            continue

        total_scores = sum(len(v) for v in agg_scores_by_cat.values())
        n_seeds = len(seed_keys)
        seed_label = f", {n_seeds} seeds" if n_seeds > 1 else ""
        name = approach_display_name(approach)
        print(f"\n{'=' * 60}")
        print(f"  {name}  ({approach_key}{seed_label})")
        print(f"{'=' * 60}")
        print(f"  Fidelity scores: {total_scores} queries")
        for cat_name, cat_scores in agg_scores_by_cat.items():
            if not cat_scores:
                continue
            s = sorted(cat_scores)
            avg = sum(s) / len(s)
            sm = seed_means_by_cat[cat_name]
            if len(sm) > 1:
                std = statistics.stdev(sm)
                print(f"    {cat_name:12s}  n={len(s):4d}  mean={avg:.1f} ±{std:.1f}  min={s[0]:.1f}  max={s[-1]:.1f}")
            else:
                print(f"    {cat_name:12s}  n={len(s):4d}  mean={avg:.1f}  min={s[0]:.1f}  max={s[-1]:.1f}")

        matching_exps = [e for e in all_experiments if approach in e.compressor_key]
        if matching_exps:
            avg_size = sum(e.size_kb for e in matching_exps) / len(matching_exps)
            avg_cpu = sum(e.cpu_time_seconds for e in matching_exps) / len(matching_exps)
            avg_gpu = sum(e.gpu_time_seconds for e in matching_exps) / len(matching_exps)
            avg_cost = sum(e.total_cost for e in matching_exps) / len(matching_exps)
            size_str = f"{avg_size:.0f} KB" if avg_size < 1024 else f"{avg_size / 1024:.1f} MB"
            time_val = avg_gpu if avg_gpu > avg_cpu else avg_cpu
            time_label = "GPU" if avg_gpu > avg_cpu else "CPU"
            print(f"  Cost breakdown ({len(matching_exps)} experiments):")
            print(f"    size       {size_str}")
            print(f"    time       {time_val:.1f} s ({time_label})")
            print(f"    cost       ${avg_cost:.4f}")


if __name__ == "__main__":
    main()
