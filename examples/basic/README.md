# TPack end-to-end demo

Bring up tracegen → edge collector → gRPC stream → backend collector → two Tempo backends → two Grafana dashboards. Side-by-side comparison of the original tracegen workload (left) and the TPack-regenerated workload (right).

## Prerequisites

- Docker Engine 24+
- Docker Compose v2

The whole stack runs in containers; nothing needs to be installed on the host.

## Run

```bash
cd examples/basic
docker compose up --build
```

First build downloads OTel SDKs, the Go toolchain image, and the otel collector deps; expect ~3 minutes on a cold cache. Subsequent `up` is seconds.

## What success looks like

After **~70 seconds** (one flush interval + a little gRPC handshake time):

1. `otelcol-edge` logs:
   ```
   Periodic flush triggered  buffer_size=NN
   Flushing traces for training  trace_count=NN  total_spans=NN
   Training complete  ...
   Model serialized  size_bytes=NNNN
   Model stored successfully
   ```
2. `otelcol-backend` logs:
   ```
   Connected to model server  endpoint=otelcol-edge:9090
   Received model from server  size_bytes=NNNN  trace_count=NN
   Loaded TPack models  vocab_size=NN
   First model received; starting generation
   Generated traces  count=NN  total_spans=NN
   ```
3. Both Tempo backends have traces:
   ```bash
   curl -s localhost:3200/api/search | jq '.traces | length'   # original
   curl -s localhost:3201/api/search | jq '.traces | length'   # TPack regenerated
   ```

## Dashboards

| URL | Purpose |
|---|---|
| http://localhost:3001 | Grafana → `tempo-original` (raw tracegen output) |
| http://localhost:3002 | Grafana → `tempo-tpack` (TPack-regenerated traces) |
| http://localhost:3200 | Tempo HTTP API (original) |
| http://localhost:3201 | Tempo HTTP API (tpack) |

Each Grafana auto-loads the **Traces** dashboard (rate / error rate / duration percentiles / top operations) on first visit; expect panels to populate ~70s after `up`. For ad-hoc lookups, **Explore** → **Tempo** → **Search** `{}` lists recent traces, or paste a trace ID under **TraceQL**.

## Architecture

```
tracegen ─OTLP/gRPC:4317→ otelcol-edge ┬─OTLP→ tempo-original ─→ grafana-original :3001
                                       │
                                       └─gRPC:9090 (model stream)→ otelcol-backend ─OTLP→ tempo-tpack ─→ grafana-tpack :3002
```

- `flush_interval_seconds: 60` (collector-edge.yaml) means the edge collector trains every minute.
- `primary_attributes` defaults to `[service.name, span.kind, operation.name, status.code]`.
- `dependent_attributes: [http.status_code]` exercises the dependent-attribute predictor path; tracegen emits this attribute (`200`, `500`, `502`, `503`).

## Stopping

```bash
docker compose down -v       # also remove Tempo block volumes
```

## Troubleshooting

**"Empty Grafana for 60+ seconds"** — Expected. The edge collector accumulates a full bucket before emitting the first model. Check `docker compose logs otelcol-edge | grep "Model serialized"` to confirm the first flush.

**Backend logs `dial model server: ...`** — Connection backoff is normal during the brief window where the edge collector's gRPC server isn't listening yet (~1 second on cold start). The receiver auto-retries with exponential backoff up to 30s.

**`go.work requires go >= 1.25.0`** — Bump the Go base image in `Dockerfile` to a `golang:1.25-*` tag. Should not happen with the shipped Dockerfile.

**No traces in `tempo-tpack`** — Check `docker compose logs otelcol-backend | grep "Generated traces"`. If absent, the gRPC subscription is not delivering models — check `otelcol-edge` logs for `gRPC server listening` and that port 9090 is reachable from the backend container (`docker compose exec otelcol-backend nc -zv otelcol-edge 9090`).

**Container restart loop** — The most common cause is config validation failing. Both configs have `Validate()` methods covering required fields and value ranges; the failing message will appear at the top of the container's log.

## Knobs

In `collector-edge.yaml` (the edge collector that compresses):

| Field | Default | Notes |
|---|---|---|
| `flush_interval_seconds` | required | Lower for faster demo turnaround (e.g. `15`). |
| `max_buffered_traces` | required | Forces a flush if the bucket fills before the interval. |
| `model_server_port` | optional | Setting `0` (or omitting) disables gRPC streaming; you must then set `output_path` so models are written to disk. |
| `dependent_attributes` | optional | Span attributes the model should regenerate (high-cardinality columns). |
| `primary_attributes` | optional | Defaults to the canonical 4. |

In `collector-backend.yaml` (the backend collector that decompresses):

| Field | Default | Notes |
|---|---|---|
| `source_type` | required | `grpc` (live stream) or `filesystem` (replay a `.pb` model file). |
| `model_server_endpoint` | grpc only | `host:port` of the edge collector's gRPC server. |
| `input_path` | filesystem only | Path inside the container to a serialized model. |
| `continuous_generation` | `false` | If `true`, in filesystem mode the receiver re-emits the same model in a loop. |
