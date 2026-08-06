"""Aggregate per-scenario CUSUM anomaly-detection metrics into a comparison
table across compressors (typically: head_1, head_100, tpack_default).

Reads `report.json` files produced by `tpack-eval --report`. Each report
contains `reports.anomaly_detection.{appName}_{compressor}.{metric}.avg` for
one scenario × run. We aggregate across all input reports per compressor
prefix and print a table with mean ± std for each metric.

Usage:
    uv run anomaly_summary --input "output/RE2/RE2-OB/*/*/report.json" \\
        --compressors head_1 head_100 tpack_default
"""

import argparse
import glob
import json
import pathlib
import statistics
import sys
from collections import defaultdict


def load_report(path: pathlib.Path) -> dict:
    with open(path) as f:
        return json.load(f)


def find_keys_for_prefix(report: dict, prefix: str) -> list[str]:
    """Find all keys in reports.anomaly_detection matching `<app>_<prefix>[_<seed_or_iter>]`.

    The prefix `head_1` matches `RE2-OB_head_1_1`, `RE2-OB_head_1_2`, `RE2-OB_head_1_3`,
    but NOT `RE2-OB_head_100_1` (since `head_100` parts != `head_1` parts).
    """
    section = report.get("reports", {}).get("anomaly_detection", {})
    if not section:
        return []
    prefix_parts = prefix.split("_")
    matched = []
    for key in section.keys():
        parts = key.split("_")
        # Look for the prefix as a contiguous sequence at any position
        for i in range(len(parts) - len(prefix_parts) + 1):
            if parts[i:i + len(prefix_parts)] == prefix_parts:
                # Verify next token (if any) is purely numeric (iter / seed)
                # so we don't conflate head_1 with head_100.
                tail = parts[i + len(prefix_parts):]
                if not tail or tail[0].isdigit():
                    matched.append(key)
                    break
    return sorted(matched)


def metric_value(entry: dict, key: str) -> float | None:
    if key not in entry:
        return None
    sub = entry[key]
    if not isinstance(sub, dict) or "avg" not in sub:
        return None
    return float(sub["avg"])


def aggregate_across_reports(
    reports: list[tuple[pathlib.Path, dict]], compressor_prefix: str
) -> dict[str, list[float]]:
    """Walk all reports, collect per-(scenario, iteration) metric values for
    the given compressor prefix. Returns {metric_name: [values]}.
    """
    accum: dict[str, list[float]] = defaultdict(list)
    for path, rep in reports:
        keys = find_keys_for_prefix(rep, compressor_prefix)
        if not keys:
            continue
        section = rep.get("reports", {}).get("anomaly_detection", {})
        for k in keys:
            entry = section.get(k, {})
            if not isinstance(entry, dict):
                continue
            for metric in (
                "detected",
                "detection_delay_buckets",
                "false_alarms_pre_inject",
                "false_alarm_rate",
                "pre_inject_buckets",
                "localized_ac1",
                "localized_ac2",
                "localized_ac3",
                "localized_ac4",
                "localized_ac5",
                "num_buckets",
            ):
                v = metric_value(entry, metric)
                if v is not None:
                    accum[metric].append(v)
    return dict(accum)


def fmt_mean_std(values: list[float], pct: bool = False) -> str:
    if not values:
        return "—"
    n = len(values)
    if n == 1:
        v = values[0]
        return f"{v * 100:.1f}%" if pct else f"{v:.2f}"
    m = statistics.mean(values)
    s = statistics.stdev(values)
    if pct:
        return f"{m * 100:.1f}±{s * 100:.1f}"
    return f"{m:.2f}±{s:.2f}"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, nargs="+",
                        help="report.json paths or globs")
    parser.add_argument("--compressors", nargs="+",
                        default=["head_1", "head_100", "tpack_default"],
                        help="Compressor prefixes (default: head_1 head_100 tpack_default)")
    parser.add_argument("--latex", default=None, help="Write a LaTeX table to this path")
    args = parser.parse_args()

    paths: list[pathlib.Path] = []
    for pat in args.input:
        expanded = sorted(glob.glob(pat, recursive=True))
        if not expanded:
            print(f"Warning: no matches for {pat}", file=sys.stderr)
            continue
        paths.extend(pathlib.Path(p) for p in expanded)
    if not paths:
        print("No input reports found", file=sys.stderr)
        sys.exit(1)

    reports = [(p, load_report(p)) for p in paths]
    print(f"Loaded {len(reports)} reports")

    results: dict[str, dict[str, list[float]]] = {}
    for comp in args.compressors:
        results[comp] = aggregate_across_reports(reports, comp)

    # --- Print summary table ---
    header = f"{'Metric':<24}" + "".join(f"{comp:>22}" for comp in args.compressors)
    print()
    print(header)
    print("-" * len(header))
    rows = [
        ("TPR (detected)",         "detected",                True),
        ("FPR (per pre-bucket)",   "false_alarm_rate",        False),
        ("FP count (pre-inject)",  "false_alarms_pre_inject", False),
        ("Mean delay (buckets)",   "detection_delay_buckets", False),
        ("AC@1 (localized)",       "localized_ac1",           True),
        ("AC@3 (localized)",       "localized_ac3",           True),
        ("AC@5 (localized)",       "localized_ac5",           True),
        ("Pre-inject buckets",     "pre_inject_buckets",      False),
        ("Total buckets",          "num_buckets",             False),
        ("N scenarios",            None,                      False),
    ]
    for label, key, pct in rows:
        row = [f"{label:<24}"]
        for comp in args.compressors:
            metrics = results[comp]
            if key is None:
                vals = metrics.get("detected", [])
                row.append(f"{len(vals):>22}")
            else:
                vals = metrics.get(key, [])
                row.append(f"{fmt_mean_std(vals, pct=pct):>22}")
        print("".join(row))

    if args.latex:
        write_latex(args.compressors, results, pathlib.Path(args.latex))
        print(f"\nLaTeX table written to {args.latex}")


def write_latex(compressors: list[str], results: dict, output_path: pathlib.Path) -> None:
    pretty = {
        "head_1": "Raw (Head 1:1)",
        "head_100": "Head 1:100",
        "tpack_default": r"\sysname",
    }
    headers = " & ".join(pretty.get(c, c) for c in compressors)
    rows = [
        ("TPR",                "detected",                True),
        ("FPR (per bucket)",   "false_alarm_rate",        True),
        ("Detection delay",    "detection_delay_buckets", False),
        ("AC@1",               "localized_ac1",           True),
        ("AC@3",               "localized_ac3",           True),
    ]
    body_lines = []
    for label, key, pct in rows:
        cells = [label]
        for comp in compressors:
            vals = results[comp].get(key, [])
            cells.append(fmt_mean_std(vals, pct=pct))
        body_lines.append(" & ".join(cells) + r" \\")

    n_col = "c" * len(compressors)
    text = "\n".join([
        r"\begin{table}[t]",
        r"  \centering",
        r"  \caption{CUSUM anomaly detection metrics (mean$\pm$std across all RE2 scenarios).}",
        r"  \label{tab:anomaly_detection}",
        f"  \\begin{{tabular}}{{l{n_col}}}",
        r"    \toprule",
        f"    Metric & {headers} \\\\",
        r"    \midrule",
        *(f"    {l}" for l in body_lines),
        r"    \bottomrule",
        r"  \end{tabular}",
        r"\end{table}",
    ]) + "\n"
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(text)


if __name__ == "__main__":
    main()
