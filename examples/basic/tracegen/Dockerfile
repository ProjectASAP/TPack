FROM golang:1.24-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -o /tracegen .

FROM debian:bookworm-slim
COPY --from=builder /tracegen /tracegen
ENTRYPOINT ["/tracegen"]
