# Changelog

## 0.1.0 — 2026-05-08

Initial public release of TPack.

### Components
- `pkg/tpackmodel` — Core models (start table, topology, root duration GMM, statistical dependent-attribute predictor) plus serialization, training, generation pipelines.
- `exporter/tpackexporter` — OpenTelemetry Collector exporter (compressor): trains models from buffered traces and broadcasts via gRPC or writes to disk.
- `receiver/tpackreceiver` — OpenTelemetry Collector receiver (decompressor): subscribes to a model stream (gRPC) or watches a file, regenerates traces, pushes downstream.
- `cmd/otelcol-tpack` — Custom OTel Collector binary bundling the two components.
- `cmd/tpack-eval` — Standalone CLI for transform / head-sample / tail-sample / sifter-sample / evaluate / dataset-stats / flatten-csv.
- `tpack_eval/` — Python evaluation framework: scorecard, paper figures, TVAE baseline.
- `examples/basic/` — Docker Compose end-to-end demo.
