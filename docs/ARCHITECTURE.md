# Architecture

## Overview

TPack is a generative compression framework for distributed traces. An edge collector fits a compact statistical model on the local trace stream; the central backend regenerates synthetic traces from the model parameters. Because the model is small — tens of kilobytes per one-minute bucket, roughly two orders of magnitude smaller than the raw OTLP — and the regenerated traces preserve the distributions operators query (rate, error rate, duration percentiles, service dependencies), this lets you keep the value of full-trace ingestion at a fraction of the cost.

## Module layout

```
pkg/tpackmodel/        Core models, proto, serialization, training + generation pipelines (Go)
exporter/tpackexporter Edge collector component: trains models, broadcasts via gRPC or writes to disk
receiver/tpackreceiver Backend collector component: subscribes to model stream, regenerates traces
cmd/otelcol-tpack      Custom OTel Collector binary bundling exporter + receiver
cmd/tpack-eval         Standalone evaluation CLI (transform, sample, evaluate, dataset-stats)
tpack_eval/            Python framework: scorecard, paper figures, TVAE baseline
```

`pkg/tpackmodel` owns the algorithm. The three callers (`tpack-eval`, `tpackexporter`, `tpackreceiver`) are thin adapters that handle their I/O specifics:

- `tpackmodel.Trace` / `tpackmodel.Span` — input traces (each caller has its own converter from OTLP / Jaeger / CSV).
- `tpackmodel.StreamingTrainer` + `tpackmodel.TrainBucket` — train the four sub-models on one bucket of traces.
- `tpackmodel.GenerateBucket` — produce `[]GeneratedSpan` from a trained `TPackModelState`.

This consolidation guarantees that any feature flag (`topology_mode`, `offset_value`, `offset_model`, `use_duration_bounds`) takes effect uniformly across all three deployments.

## The four sub-models

Each module trains independently on the same input traces, per 1-minute bucket. All four live in `pkg/tpackmodel`.

| Sub-model | File | What it does |
|---|---|---|
| **Start table** $S$ | `start_table_model.go` | Exact counts per root span signature. No generative model needed at the root. |
| **Topology** | `topology_model.go` | Edge probability table $E$: $P(\text{child} \mid \text{parent}, \text{position})$. Log-potentials per (parent, child, position) triple. `topology_mode: template` switches to whole-tree memorization. |
| **Root timing** $T_r$ | `root_duration_model.go` | Gaussian mixture (up to 3 components) over root span duration, conditioned on the root signature. |
| **Child timing + dependent attributes** | `statistical_dependent_attribute_predictor.go` | OLS regression for gap / duration ratios per (parent, child) pair, plus per-pair categorical distributions over the dependent attributes. |

Per-trace training (`pkg/tpackmodel/training_per_trace.go`) updates all four modules from a single indexed trace; `StreamingTrainer.AddTrace` calls each in sequence. After every trace is added, `StreamingTrainer.Finalize` fits GMMs, builds the topology candidate cache, and finalizes the dependent-attribute predictor — these three steps run in parallel.

## Generation

`pkg/tpackmodel/generation.go` plus `generation_shard.go` is sequential per-trace, level-batched across traces:

1. Sample roots from the start table.
2. BFS expand using the topology model (or replay a memorized template in `template` mode).
3. Sample root durations from the GMM.
4. Walk the tree top-down, sampling child gaps and durations from the regression model.
5. Sample dependent-attribute values from the categorical distributions.

The whole bucket is sharded across `runtime.NumCPU()` workers; each worker has an independent RNG seeded from `RandomSeed + BucketKey*31 + worker_index`. Output is deterministic given `(RandomSeed, BucketKey)`.

## Span signatures and primary attributes

A "span signature" is the tuple of selected primary attributes. Typical defaults: `service.name`, `operation.name`, `span.kind`, `status.code`. Cardinality threshold: attributes with ≤50 unique values become primary attributes; >50 become dependent attributes modeled by per-pair empirical distributions.

The choice of primary attributes is the main fidelity-vs-cost knob. More primary attributes → more nodes in the graph → finer-grained queries can be reproduced, but model size and sparsity grow. The paper's `feat-ablation` experiment sweeps this. See `configs/otel_demo.yaml` for the canonical choice (22 primary attributes).

## OTel integration

- **Exporter** (compressor, `exporter/tpackexporter`): receives OTLP spans, buffers by trace ID, flushes on a configurable interval (`flush_interval_seconds`, default 120s) or when `max_buffered_traces` is hit. Each flush calls `tpackmodel.TrainBucket` and serializes the resulting state via `state.Marshal()`. The serialized bytes are written to disk (`output_path`) and/or broadcast over gRPC (`model_server_port > 0`).
- **Receiver** (generator, `receiver/tpackreceiver`): subscribes to a model stream (gRPC source by default) or watches a file (`source_type: filesystem`). On each new model it calls `LoadFromProto`, then `tpackmodel.GenerateBucket`, and pushes generated traces downstream via the OTel consumer in 500-trace gRPC chunks.
- **gRPC streaming**: `pkg/tpackmodel/proto/model_service.proto` defines a server-streaming `StreamModels` RPC. The exporter is the server; the receiver subscribes with exponential backoff (1s → 30s).
- **Drop-in**: no application instrumentation changes. Application code keeps emitting OTLP; only the collector deployment changes.

## Serialization

Models serialize as Protocol Buffers (`pkg/tpackmodel/proto/tpack.proto`) and are gzip-compressed in transit / at rest. Measured on otel-demo (1.84 M spans, 35 one-minute buckets): 769 KB of model in total — 22 KB per bucket on average, 37 KB at the largest — against 81 MB of gzipped OTLP, so about 106× compression. Both terms move with the dataset and the configured attribute set.

The wire format includes a delta-vocab encoding for the node dictionary so successive buckets only need to ship new entries — important when model streams run continuously over hours.

## Evaluation pipeline

```
Transform (OTLP / Jaeger / CSV → chunked OTLP dir)
  → Head sampling (1:N, 3 iterations)
  → Tail sampling (error+p95 biased, 1 run)
  → Hindsight sampling (same bias, subset cost, 1 run)
  → Sifter sampling (learned biased, tail-based cost, N iterations)
  → TPack compression (3 seeds)
  → TVAE baseline (flatten → train → reconstruct)
  → Evaluate (MAPE fidelity per query group)
  → Reports (report.json) → Scorecard / Figures
```

Cost model: $0.10/GB transmission + $0.16/hr CPU + $0.38/hr GPU.

All sampling approaches (head, tail, hindsight, sifter, TPack, TVAE) write 4 canonical timing files to `compressed/data/`:
- `compression_cpu_time_seconds`
- `compression_gpu_time_seconds`
- `decompression_cpu_time_seconds`
- `decompression_gpu_time_seconds`

Disk I/O is excluded from timing. TPack uses batched 2-phase measurement: chunks are processed in batches of 200 (configurable). Phase 1 reads chunks; Phase 2 trains. Only Phase 2 wall time counts.

## Transform pipeline

All input formats go through one unified pipeline in `cmd/tpack-eval/format_converter.go`:
- CSV (RE2) → `readCSVFile` → `ptrace.Traces`
- Jaeger dir (Uber) → `readJaegerFile` → `ptrace.Traces`
- OTLP JSONL dir (otel-demo) → `readJSONLFile` → `ptrace.Traces`

Pass 1 indexes spans, pass 2 writes a chunked OTLP directory (binary protobuf `.pb` files, one per minute bucket, optionally split by `--max-spans-per-chunk`). The `--remap` flag (Uber only) shifts traces into a single 60-second window for evaluation.

## What's NOT in TPack

- A new tracing backend. TPack pushes regenerated OTLP into whatever you already have (Tempo, Jaeger, Zipkin, vendor backends).
- A sampler. Head and tail samplers drop traces; TPack ships *every* trace, encoded as a model.
- A drop-in replacement for trace storage. The paper-quality fidelity targets are **distributional** (rate, error rate, percentile latency, dependency edges), not byte-exact. If you need exact traces for individual debugging, complement TPack with a low-rate head-sampling tap.
