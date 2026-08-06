# Datasets

The paper evaluates TPack on three publicly available trace datasets. None ship with this repo. Place the downloaded files under `data/` (or override with `DATA_DIR`); the experiment driver expects this layout:

```
$DATA_DIR/                          # default: data/  (relative to TPack repo root)
├─ otel-demo/                       # OTel Demo OTLP captures
├─ RE2/                             # RE2 fault-injection scenarios
└─ uber-trace1/                     # Uber Jaeger JSON
```

The fastest path is `bash scripts/download_data.sh` which fetches all hosted artifacts into `$DATA_DIR`. Per-dataset details follow.

## OpenTelemetry Demo

The standard OpenTelemetry Demo (the multi-service e-commerce app, 17 services).

**Stats**: 17 services, 184K traces, 1.8M spans, ~1.4 GB raw OTLP JSONL, ~48 min window.

### Download (recommended)

Hosted on Zenodo as *TPack: OpenTelemetry Demo trace dataset* — [10.5281/zenodo.20088885](https://doi.org/10.5281/zenodo.20088885).

```bash
DATA_DIR="${DATA_DIR:-data}"
mkdir -p "$DATA_DIR"
curl -L -o /tmp/otel-demo.tar.zst https://zenodo.org/records/20088885/files/otel-demo.tar.zst
echo "927d7e5bfb1b017e72d65803c1e33ca0a5778017d52bf7cbb98315df7907c177  /tmp/otel-demo.tar.zst" | sha256sum -c
zstd -d -c /tmp/otel-demo.tar.zst | tar -x -C "$DATA_DIR"
# → $DATA_DIR/otel-demo/traces-2025-10-04T*.json
```

After extraction `$DATA_DIR/otel-demo/` contains 14 JSONL files, 1.4 GB total (measured). The files are collector rotations rather than fixed time windows, so they vary in size and in the interval they cover; `traces.json` holds the tail of the capture.

### Capture your own (optional)

If you want a fresh capture instead of the published artifact:

```bash
git clone https://github.com/open-telemetry/opentelemetry-demo
cd opentelemetry-demo
# Configure the demo's collector to write OTLP JSONL files
# (see the demo repo's docs for the file exporter setup)
docker compose up
# Capture for ~48 minutes, then archive the OTLP JSONL files into $DATA_DIR/otel-demo/
```

Primary attributes used in the paper: 22 (cardinality ≤ 50 each). See `configs/otel_demo.yaml`.

## RE2 (fault-injection benchmark)

RE2 is part of **RCAEval** (*A Benchmark for Root Cause Analysis of Microservice Systems*), published on Zenodo as [10.5281/zenodo.14590730](https://doi.org/10.5281/zenodo.14590730) under CC BY 4.0. Two of its systems carry distributed traces and are evaluated here:

- **RE2-TT** (Train Ticket): 27 services
- **RE2-OB** (Online Boutique): 7 services

The third system, **RE2-SS** (Sock Shop), ships no traces and is not used; RE1/RE3 from the same record are also unused.

Each scenario has multiple runs (numbered `1`, `2`, `3`, …); faults are injected into specific services with the inject time recorded for change-detection RCA.

**Stats per run**:
- RE2-TT: 6.4K traces, 748K spans, 109 MB
- RE2-OB: 23.5K traces, 383K spans, 67 MB

### Download (recommended)

```bash
bash scripts/download_data.sh re2
```

This fetches `RE2-TT.zip` (2.8 GB) and `RE2-OB.zip` (1.2 GB) from the Zenodo record, verifies their MD5 sums, and normalizes the extracted tree into the layout below under `$DATA_DIR/RE2/`.

Budget disk for the extracted trees, not just the downloads — the script keeps
only `traces.csv` and `inject_time.txt`, and those still come to **9.6 GB for
RE2-TT and 6.0 GB for RE2-OB** (measured), plus the two zips in `$TMP_DIR` while
it runs. Set `TMP_DIR` if `/tmp` is a small tmpfs.

Manual fallback (per system):
```bash
DATA_DIR="${DATA_DIR:-data}"
curl -L -o /tmp/RE2-OB.zip https://zenodo.org/api/records/14590730/files/RE2-OB.zip/content
echo "b9e23f8842c404b396ffd2becff15de4  /tmp/RE2-OB.zip" | md5sum -c   # RE2-TT: a7fbcd1ada406067dcc50771ae398408
unzip -q /tmp/RE2-OB.zip -d "$DATA_DIR/RE2/"   # then ensure cases sit at RE2/RE2-OB/<service>_<fault>/<run>/traces.csv
```

Layout:
```
data/RE2/
├─ RE2-TT/
│  ├─ <service>_<fault>/
│  │  ├─ 1/{traces.csv, inject_time.txt, metrics.json, logs.csv}
│  │  ├─ 2/...
│  │  └─ 3/...
│  └─ ...
└─ RE2-OB/<same structure>
```

`tpack-eval` reads `traces.csv` (RE2 CSV schema). Each `inject_time.txt` is a single Unix timestamp (seconds); the evaluator splits spans into normal (before) and fault (after) periods.

## Uber distributed-system traces

Uber production microservice traces, from *The Tale of Errors in Microservices* (SIGMETRICS 2025). Published on Zenodo as [10.5281/zenodo.13947828](https://doi.org/10.5281/zenodo.13947828) (Artifact part 1) under CC BY 4.0.

**Format**: Jaeger JSON, one trace per file under `data/uber-trace1/`.

### Download (recommended)

```bash
bash scripts/download_data.sh uber1
```

The `trace1` archive is split into ~92 binary parts (`trace1_aa` … `trace1_dn`, ~21 GB total) that are pieces of one zstd-compressed tar. The script reads the per-part MD5s from the Zenodo manifest, downloads and verifies each part, then streams them through `zstd | tar` into `data/uber-trace1/`. **Extraction needs ~300–500 GB of free disk.** The `driver-sanitized.tar.zst` (App-Launch use case) in the same record and the `trace2` set in the separate record [10.5281/zenodo.13952897](https://doi.org/10.5281/zenodo.13952897) are not used here.

After extraction, point `--transform --input` at the directory containing the `*.json` trace files (the extracted tree under `data/uber-trace1/`).

Because the full dataset is huge, the paper's fidelity evaluation uses a 20K-trace subset shifted into a single 60s window via:

```bash
./tpack-eval --transform --config configs/uber.yaml \
  --input data/uber-trace1 --output data/uber-trace1-transformed \
  --remap --max-traces 20000 --max-spans-per-chunk 100000
```

The `--remap` flag is Uber-specific: it shifts all trace start times into [0, 60s), discards root spans longer than 60s, and writes everything to bucket 0. After remap, ~17.8K traces / ~17M spans remain.

The scalability experiment (paper's Figure 11) sweeps sizes 1K–50K using:

```bash
for N in 1000 2000 5000 10000 20000 50000; do
  ./tpack-eval --transform --config configs/uber.yaml \
    --input data/uber-trace1 --output data/uber-${N} \
    --remap --max-traces ${N} --max-spans-per-chunk 100000
done
```

## Custom datasets

Three input formats are supported by `tpack-eval --transform`:

| Format | Detected from | Layout |
|---|---|---|
| OTLP JSONL | `*.jsonl` files in input dir | One JSON-encoded `ExportTraceServiceRequest` per line |
| Jaeger JSON | `*.json` files (one trace per file) | `--remap` recommended for evaluation |
| CSV (RE2) | `*.csv`, or a dir of scenario subdirs holding `traces.csv` | RE2 schema: `traceID,spanID,parentSpanID,serviceName,methodName,operationName,startTime,duration,...` |

Output is always a chunked OTLP binary protobuf directory under `output/<name>/<n>/`, ready for `--evaluate-only`.

To use a custom dataset:
1. Drop it under `data/<your-name>/`.
2. Create a YAML config under `configs/<your-name>.yaml` listing the primary and dependent attributes you want.
3. Add a part to `scripts/run_all_experiments.sh`, or just invoke `tpack-eval` directly.

## Disk and memory budget

| Step | Disk | Peak RAM |
|---|---|---|
| Transform | up to 2× input size (raw + chunked OTLP) | ~4 GB for 20K Uber traces |
| Train (TPack) | tens of KB per minute bucket (measured on otel-demo: 22 KB mean, 37 KB max) | ~4 GB for 1.8M-span otel-demo bucket |
| Train (TVAE) | ~5 MB / minute bucket | ~4 GB CPU + 8 GB GPU |
| Evaluate | see below — `report.json` scales with dataset size | < 2 GB |

**Budget generously for `output/`.** It is much larger than the model sizes
above suggest, because every approach writes out its full regenerated or sampled
dataset alongside the report. Measured:

| Part | Output | of which `report.json` |
|---|---|---|
| `otel-demo` | **6.6 GB** | **2.7 GB** |
| `re2` | **~226 MB per scenario** across 180 scenarios (2 systems × 30 service/fault dirs × 3 runs) → **~40 GB** | ~13 MB each |

`report.json` grows with the number of spans evaluated, so it is ~13 MB for a
single RE2 scenario but 2.7 GB for the 1.8M-span otel-demo run. A full
`otel-demo` + `re2` pass needs roughly **50 GB** of free space. Point
`OUTPUT_DIR` at a large filesystem rather than filling the repo checkout.
