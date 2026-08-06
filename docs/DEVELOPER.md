# Developer guide

This is for engineers wiring TPack into a real OpenTelemetry collector deployment, or extending it with new predictors / pipeline steps.

## Mental model

TPack is two OTel Collector components connected by a gRPC stream:

```
[ application ] ──OTLP──▶ [ edge collector with tpackexporter ]
                                    │
                                    │ gRPC stream of trained models (~kB/min)
                                    ▼
                          [ backend collector with tpackreceiver ] ──OTLP──▶ [ Tempo / Jaeger / vendor backend ]
```

The exporter never forwards spans — it accumulates them, trains a statistical model every flush interval, and broadcasts the serialized model. The receiver subscribes to the stream, regenerates synthetic traces, and pushes them downstream like any other OTLP receiver.

## Deployment patterns

### 1. Edge ↔ backend across an expensive network link

The case the paper targets: spans cost money to ship across regions or out of clouds, and you need full coverage rather than head-sampled traces. Run an edge collector inside each region with `tpackexporter`, terminate the gRPC model stream at one (or a few) backend collectors that run `tpackreceiver`, and forward the regenerated traces into your existing pipeline.

Edge config:
```yaml
exporters:
  tpack:
    flush_interval_seconds: 60
    max_buffered_traces: 50000
    model_server_port: 9090
    primary_attributes:
      - service.name
      - operation.name
      - span.kind
      - status.code
    dependent_attributes:
      - http.status_code
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [tpack]
```

Backend config:
```yaml
receivers:
  tpack:
    source_type: grpc
    model_server_endpoint: edge-collector.region-us.internal:9090
exporters:
  otlp:
    endpoint: tempo:4317
service:
  pipelines:
    traces:
      receivers: [tpack]
      exporters: [otlp]
```

### 2. Storage compression (filesystem mode)

For long-term archival — keep models on disk, regenerate on demand:

Edge:
```yaml
exporters:
  tpack:
    flush_interval_seconds: 60
    output_path: /var/lib/tpack/models    # write each bucket's model
    model_server_port: 0                  # disable gRPC streaming
```

Replay:
```yaml
receivers:
  tpack:
    source_type: filesystem
    input_path: /var/lib/tpack/models/2026-05-08T12-00.pb
    continuous_generation: false
```

### 3. Tap (head sampling complement)

TPack reproduces distributional queries; if you need exact individual traces (live debugging), run a low-rate head sampler in parallel and union the two streams in your backend. The two pipelines share the same edge collector.

## Knobs that matter

| Setting | Where | Default | When to change |
|---|---|---|---|
| `flush_interval_seconds` | tpackexporter | 120 | Lower = lower latency between event and visibility, higher CPU cost. Demo uses 60. |
| `max_buffered_traces` | tpackexporter | 1000000 | Forces flush if bucket fills before interval (e.g. spike). |
| `primary_attributes` | YAML config (loaded by edge collector via `--config`) | service.name, operation.name, span.kind, status.code | Add attributes to capture finer-grained queries; cardinality budget is ~22 attrs (see `configs/otel_demo.yaml`). |
| `dependent_attributes` | YAML config | empty | High-cardinality attributes you want regenerated. Each adds a per-pair categorical distribution. |
| `topology_mode` | exporter config | `edge` | `template` memorizes whole tree shapes; trades model size for exact tree fidelity. |
| `offset_value` / `offset_model` | exporter config | `ratio` / `regression` | How child timing depends on parent. Don't change unless ablating. |
| `random_seed` | exporter config | 42 | For deterministic regeneration. |
| `source_type` | tpackreceiver | `grpc` | Switch to `filesystem` for replay-from-disk. |

For the full list see [`docs/CONFIG.md`](CONFIG.md).

## Extending TPack

### Add a new dependent-attribute predictor

The `DependentAttributePredictor` interface lives in `pkg/tpackmodel/dependent_attribute_predictor.go`. The shipped implementation is `StatisticalDependentAttributePredictor` (per-pair empirical distributions + OLS regression for timing). To add another:

1. Implement the interface in a new file under `pkg/tpackmodel/`.
2. Register it in `pkg/tpackmodel/config.go` under the `metadata_predictor` switch (the field name in YAML is currently `metadata_predictor`; selecting your impl is the only place "metadata" terminology survives).
3. Update `pkg/tpackmodel/proto/tpack.proto` only if you need new serialized fields, then run `make proto-gen`.

### Add a new evaluator

`cmd/tpack-eval/eval_*.go` files each implement one query category (rate, error, duration, graph, RCA, span-count, timing). To add a new fidelity metric:

1. Create `cmd/tpack-eval/eval_{name}.go` exporting a `runEval{Name}` function.
2. Wire it into `runEvaluate` in `cmd/tpack-eval/evaluator.go`.
3. Add a section to `report.go` if you want it to appear in `report.json`.

### Add a new format converter

Input formats live in `cmd/tpack-eval/format_converter.go` (formerly `transformer.go`). Adding a new format means adding a `read{Format}File` function and a case in the dispatcher. The output is always a chunked OTLP `.pb` directory.

## Performance notes

- `tpack-eval --transform` is single-threaded reading + multi-threaded writing. Use `--max-spans-per-chunk 100000` for better parallelism downstream.
- `StreamingTrainer.AddTrace` is sequential. If you want to parallelize, partition by trace ID and merge with `tpackmodel.MergeTrainers`.
- `GenerateBucket` shards across `runtime.NumCPU()` by default. Each shard is independent given a deterministic seed.
- The GMM fit and topology cache build run in parallel during `Finalize`; they're the bottlenecks for large bucks.
