# TPack

> **NSDI '27 artifact reviewers:** start at [`docs/AE.md`](docs/AE.md), and
> evaluate the **`nsdi27-ae`** tag.

**TPack** is a generative compression framework for distributed traces. Instead of forwarding every span across the network, an edge collector fits a compact statistical model on the local trace stream, transmits only the model parameters, and a backend collector regenerates synthetic traces that preserve the queries operators actually run, e.g. rate, error rate, latency percentiles, and service dependency structure.

The core idea: traces are a probability graph. TPack learns the graph (start table, edge probabilities), the durations conditioned on graph position (Gaussian mixtures + regression), and the dependent attributes (categorical predictors). At generation time it takes a random walk through that graph, materializes durations and attributes, and emits the result as OTLP — drop-in compatible with any tracing backend.

## TL;DR demo

```bash
git clone https://github.com/ProjectASAP/TPack.git
cd TPack/examples/basic
docker compose up --build
```

After ~70 seconds, browse to:
- http://localhost:3001 — Grafana over the original tracegen workload
- http://localhost:3002 — Grafana over the TPack-regenerated workload

The two should look similar in shape, with the TPack side reconstructed from a model that was ~kilobytes in size.

See [`examples/basic/README.md`](examples/basic/README.md) for what the logs should say and how to interpret the side-by-side dashboards.

## Two paths

TPack has two distinct audiences. Pick yours:

### A. I want to integrate TPack into my observability stack

Drop the TPack exporter into your edge collector and the TPack receiver into your backend collector. The exporter accumulates spans for `flush_interval_seconds`, trains a model, and broadcasts it over gRPC; the receiver subscribes, regenerates traces, and forwards them to your existing pipeline.

Edge collector (compresses):
```yaml
exporters:
  tpack:
    flush_interval_seconds: 60
    max_buffered_traces: 50000
    model_server_port: 9090
    primary_attributes:
      - service.name
      - span.kind
      - operation.name
      - status.code
    dependent_attributes:
      - http.status_code

service:
  pipelines:
    traces/compress:
      receivers: [otlp]
      exporters: [tpack]
```

Backend collector (decompresses):
```yaml
receivers:
  tpack:
    source_type: grpc
    model_server_endpoint: edge-collector:9090
    continuous_generation: true

exporters:
  otlp_grpc:
    endpoint: tempo:4317   # or your jaeger / tempo / OTLP backend
    tls:
      insecure: true

service:
  pipelines:
    traces/decompress:
      receivers: [tpack]
      exporters: [otlp_grpc]
```

For details see [`docs/DEVELOPER.md`](docs/DEVELOPER.md), [`docs/CONFIG.md`](docs/CONFIG.md), and [`examples/basic/`](examples/basic/).

### B. I want to reproduce the paper's experiments

The paper evaluates TPack on three datasets (OTel Demo, RE2, Uber) across 11 experiment parts: main result, feature/node/graph ablations, RCA, scalability, TVAE baseline, and figure generation.

```bash
uv sync --frozen                                 # install Python eval framework from uv.lock
make build                                       # build tpack-eval Go binary
bash scripts/download_data.sh otel-demo         # ~60 MB → 1.4 GB unpacked into data/otel-demo/
bash scripts/run_all_experiments.sh otel-demo   # run one part
bash scripts/run_all_experiments.sh             # run everything
uv run plot_paper --mode query_fidelity --report output/otel-demo/report.json
uv run scorecard --input output/otel-demo/report.json --approaches tpack_default
```

See [`docs/AE.md`](docs/AE.md) for environment setup (Go 1.25, Python 3.13, Docker), per-dataset prerequisites, expected runtimes, and how each paper claim maps to a command.

## Repo layout

```
TPack/
├─ pkg/tpackmodel/              # core models + algorithms (Go)
├─ exporter/tpackexporter/      # OTel Collector exporter (compressor)
├─ receiver/tpackreceiver/      # OTel Collector receiver (decompressor)
├─ cmd/
│  ├─ otelcol-tpack/            # custom OTel Collector binary
│  └─ tpack-eval/               # standalone evaluation CLI
├─ examples/basic/              # docker compose end-to-end demo
├─ tpack_eval/                  # Python evaluation framework (paper figures, scorecard, TVAE baseline)
├─ configs/                     # per-dataset YAML configs + ablations
├─ scripts/                     # experiment driver, smoke test
└─ docs/                        # architecture, reproduction, config reference
```

## Build matrix

| Component | Required | Notes |
|---|---|---|
| Go | 1.25.0 | pinned in `go.work` and `Dockerfile` |
| Python | 3.13 | pinned in `.python-version` |
| protoc | 3.21+ | + `protoc-gen-go` and `protoc-gen-go-grpc` |
| Docker | 24+ | only for the `examples/basic/` demo |
| uv | recent | recommended Python package manager |

Per-OS install commands are in [`docs/AE.md`](docs/AE.md#toolchain).

## Datasets

The paper evaluates on three public traces. None ship with the repo; download instructions live in [`docs/DATASETS.md`](docs/DATASETS.md).

| Dataset | Source | Spans | Window |
|---|---|---|---|
| OTel Demo | `opentelemetry-demo` running 17 services | 1.8M | ~48 min |
| RE2 | RE2 fault-injection benchmark | 748K/run | ~24 min |
| Uber | Uber distributed-systems traces | 746M | multi-day |

## How TPack works (one paragraph)

For each 1-minute bucket of traces, TPack trains four sub-models in parallel:
1. **Start table** — exact counts of each root span signature.
2. **Topology** — edge probabilities `P(child signature | parent signature, child position)`.
3. **Root timing** — Gaussian mixture (up to 3 components) over root span duration.
4. **Child timing + dependent attributes** — OLS regression for gap/duration ratios + per-pair categorical distributions for high-cardinality attributes.

Generation is a level-batched BFS over a random walk through the topology graph, parallelized across CPU cores. Models serialize to gzipped protobuf — tens of kilobytes per one-minute bucket, roughly two orders of magnitude smaller than the raw OTLP they replace. Exact sizes depend on the dataset and the configured attribute set; run `scripts/run_all_experiments.sh otel-demo` to measure them for yourself.

For more depth see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Citation

```bibtex
@inproceedings{tpack2026,
  title     = {T-PACK: Structure-Aware Generative Compression for Efficient Distributed Tracing},
  author    = {Chin, Yen-Ru and Srivastava, Milind and Zhou, Yajie and Fanti, Giulia and Sekar, Vyas},
  booktitle = {22nd USENIX Symposium on Networked Systems Design and Implementation (NSDI '27)},
  publisher = {USENIX Association},
  year      = {2027},
}
```

See `CITATION.cff` for machine-readable metadata.

## License

MIT — see [`LICENSE`](LICENSE).

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md).
