"""Collect scalability experiment results into JSON for paper figures/tables.

All metrics are read from files written by the transform and TPack pipelines.
No separate gzip benchmark is needed.
"""

import argparse
import json
import os


def _read_float(path):
    with open(path) as f:
        return float(f.read().strip())


def _read_int(path):
    with open(path) as f:
        return int(f.read().strip())


def main():
    parser = argparse.ArgumentParser(description="Collect uber scalability results")
    parser.add_argument("--data-dir", required=True, help="Base dir for transformed data (e.g. data/uber-scalability)")
    parser.add_argument("--output-dir", required=True, help="Base dir for experiment output (e.g. output/uber-scalability)")
    parser.add_argument("--out", required=True, help="Output JSON path (e.g. data/paper/fig11_scalability.json)")
    parser.add_argument("--scales", default="10000,20000,50000,100000,200000,500000",
                        help="Comma-separated trace counts")
    args = parser.parse_args()

    scales = [int(s) for s in args.scales.split(",")]

    results = []
    for N in scales:
        tpack_base = os.path.join(args.output_dir, str(N), "tpack_1", "compressed", "data")
        data_dir = os.path.join(args.data_dir, str(N))

        # Check both TPack output and transform output exist
        if not os.path.exists(os.path.join(tpack_base, "compression_cpu_time_seconds")):
            print(f"  SKIP {N} (no TPack results)")
            continue
        if not os.path.exists(os.path.join(data_dir, "raw_bytes")):
            print(f"  SKIP {N} (no transform metrics — retransform needed)")
            continue

        # From transform (data dir)
        raw_bytes = _read_int(os.path.join(data_dir, "raw_bytes"))
        raw_gz_bytes = _read_int(os.path.join(data_dir, "raw_gz_bytes"))
        gz_comp_s = _read_float(os.path.join(data_dir, "raw_gzip_compress_seconds"))
        gz_decomp_s = _read_float(os.path.join(data_dir, "raw_gzip_decompress_seconds"))

        # From TPack (output dir)
        comp_cpu = _read_float(os.path.join(tpack_base, "compression_cpu_time_seconds"))
        decomp_cpu = _read_float(os.path.join(tpack_base, "decompression_cpu_time_seconds"))
        total = comp_cpu + decomp_cpu

        model_path = os.path.join(tpack_base, "model_bucket_0")
        model_gz_bytes = os.path.getsize(model_path) if os.path.exists(model_path) else 0

        model_raw_path = os.path.join(tpack_base, "model_raw_bytes")
        model_raw_bytes = _read_int(model_raw_path) if os.path.exists(model_raw_path) else 0

        traces = _read_int(os.path.join(tpack_base, "input_traces")) if os.path.exists(os.path.join(tpack_base, "input_traces")) else 0
        spans = _read_int(os.path.join(tpack_base, "input_spans")) if os.path.exists(os.path.join(tpack_base, "input_spans")) else 0

        ratio = raw_gz_bytes / model_gz_bytes if model_gz_bytes > 0 else 0
        print(f"  {traces:>6} traces | {spans / 1e6:>6.1f}M spans | "
              f"raw={raw_bytes / 1e9:>.1f}GB gz={raw_gz_bytes / 1e6:>.0f}MB | "
              f"model={model_raw_bytes / 1e6:>.0f}MB gz={model_gz_bytes / 1e6:>.0f}MB | "
              f"ratio={ratio:>.0f}x | total={total:>.1f}s")

        results.append({
            "traces": traces,
            "spans_millions": round(spans / 1e6, 1),
            "compression_seconds": round(comp_cpu, 1),
            "decompression_seconds": round(decomp_cpu, 1),
            "total_seconds": round(total, 1),
            "raw_bytes": raw_bytes,
            "raw_gz_bytes": raw_gz_bytes,
            "raw_gzip_compress_seconds": round(gz_comp_s, 1),
            "raw_gzip_decompress_seconds": round(gz_decomp_s, 1),
            "model_raw_bytes": model_raw_bytes,
            "model_gz_bytes": model_gz_bytes,
        })

    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    with open(args.out, "w") as f:
        json.dump(results, f, indent=2)
        f.write("\n")
    print(f"  Wrote {len(results)} entries to {args.out}")


if __name__ == "__main__":
    main()
