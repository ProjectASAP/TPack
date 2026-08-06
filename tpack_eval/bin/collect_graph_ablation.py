#!/usr/bin/env python3
"""Collect graph ablation data for fig13.

For each uber size N, reads:
  - output/uber-graph-ablation/{N}/report.json (fidelity, cost per compressor)
  - data/uber-scalability/{N}/stats.json (unique template count)

Emits JSON with two series (edge, template), each a list of points
{"N", "fidelity", "cost", "templates"}.
"""

import argparse
import orjson
from pathlib import Path

from tpack_eval.plotting.data import ReportParser, CostConfig


def collect_mean_fidelity(report_data, compressor_key):
    """Copy of plot_paper.collect_mean_fidelity — same metric definition."""
    import numpy as np
    metric_means = []
    r = report_data["reports"]

    rate = r.get("rate_over_time", {}).get(compressor_key, {})
    rate_fids = [d["mape_fidelity"] for d in rate.values() if isinstance(d, dict) and "mape_fidelity" in d]
    if rate_fids:
        metric_means.append(np.mean(rate_fids))

    err = r.get("error_over_time", {}).get(compressor_key, {})
    err_fids = [d["mape_fidelity"] for d in err.values() if isinstance(d, dict) and "mape_fidelity" in d]
    if err_fids:
        metric_means.append(np.mean(err_fids))

    dur_fids = []
    for pct in ("duration_over_time_p50", "duration_over_time_p90", "duration_over_time_p99", "duration_over_time"):
        d = r.get(pct, {}).get(compressor_key, {})
        dur_fids.extend([v["mape_fidelity"] for v in d.values() if isinstance(v, dict) and "mape_fidelity" in v])
    if dur_fids:
        metric_means.append(np.mean(dur_fids))

    graph = r.get("graph", {}).get(compressor_key, {})
    graph_fids = [v["fidelity"] for v in graph.values() if isinstance(v, dict) and "fidelity" in v]
    if graph_fids:
        metric_means.append(np.mean(graph_fids))

    if not metric_means:
        return None
    return float(np.mean(metric_means))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-root", type=Path, required=True,
                    help="data/uber-scalability (contains {N}/stats.json)")
    ap.add_argument("--output-root", type=Path, required=True,
                    help="output/uber-graph-ablation (contains {N}/report.json)")
    ap.add_argument("--sizes", required=True,
                    help="Comma-separated list of N, e.g. 1000,2000,5000,10000,20000,50000")
    ap.add_argument("--out", type=Path, required=True,
                    help="Output JSON path, e.g. data/paper/fig13_graph_ablation.json")
    args = ap.parse_args()

    sizes = [int(s) for s in args.sizes.split(",")]
    parser = ReportParser(CostConfig())

    result = {"sizes": sizes, "edge": [], "template": []}

    for N in sizes:
        report_path = args.output_root / str(N) / "report.json"
        stats_path = args.data_root / str(N) / "stats.json"

        if not report_path.exists():
            print(f"  skip N={N}: {report_path} missing")
            continue

        with open(report_path, "rb") as f:
            report_data = orjson.loads(f.read())
        experiments = parser.parse_report_data(report_data)

        templates = None
        edges = None
        spans = None
        if stats_path.exists():
            with open(stats_path, "rb") as f:
                stats = orjson.loads(f.read())
            templates = stats.get("unique_templates")
            edges = stats.get("unique_edges")
            spans = stats.get("spans")

        import numpy as np
        # Group experiments by name ("default" / "template") so we can aggregate
        # across seeds (3 seeds → mean ± std on both axes).
        grouped = {"default": [], "template": []}
        for exp in experiments:
            if exp.experiment_name not in grouped:
                continue
            fid = collect_mean_fidelity(report_data, exp.compressor_key)
            if fid is None:
                continue
            cost_per_bspans = None
            if spans and spans > 0:
                cost_per_bspans = exp.total_cost / (spans / 1e9)
            grouped[exp.experiment_name].append((fid, exp.total_cost, cost_per_bspans))

        for exp_name, series_key in (("default", "edge"), ("template", "template")):
            runs = grouped[exp_name]
            if not runs:
                continue
            fids = np.array([r[0] for r in runs])
            costs = np.array([r[1] for r in runs])
            cpbs = np.array([r[2] for r in runs if r[2] is not None])
            pt = {
                "N": N,
                "fidelity": float(fids.mean()),
                "fidelity_std": float(fids.std(ddof=0)),
                "cost": float(costs.mean()),
                "cost_std": float(costs.std(ddof=0)),
                "cost_per_bspans": float(cpbs.mean()) if cpbs.size else None,
                "cost_per_bspans_std": float(cpbs.std(ddof=0)) if cpbs.size else None,
                "seeds": len(runs),
                "spans": spans,
                "templates": templates,
                "edges": edges,
            }
            result[series_key].append(pt)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with open(args.out, "wb") as f:
        f.write(orjson.dumps(result, option=orjson.OPT_INDENT_2))
    print(f"Wrote {args.out}")


if __name__ == "__main__":
    main()
