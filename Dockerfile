FROM golang:1.25-bookworm AS builder

WORKDIR /src

# Copy go.work and all module go.mod/go.sum files for caching
COPY go.work ./
COPY pkg/tpackmodel/go.mod pkg/tpackmodel/go.sum ./pkg/tpackmodel/
COPY exporter/tpackexporter/go.mod exporter/tpackexporter/go.sum ./exporter/tpackexporter/
COPY receiver/tpackreceiver/go.mod receiver/tpackreceiver/go.sum ./receiver/tpackreceiver/
COPY cmd/otelcol-tpack/go.mod cmd/otelcol-tpack/go.sum ./cmd/otelcol-tpack/
COPY cmd/tpack-eval/go.mod cmd/tpack-eval/go.sum ./cmd/tpack-eval/

# Download dependencies
RUN go work sync && \
    cd cmd/otelcol-tpack && go mod download

# Copy source
COPY . .

RUN go build -o /otelcol-tpack ./cmd/otelcol-tpack

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /otelcol-tpack /otelcol-tpack

EXPOSE 4317 9090

ENTRYPOINT ["/otelcol-tpack"]
