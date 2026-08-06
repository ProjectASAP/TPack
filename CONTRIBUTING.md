# Contributing to TPack

Thanks for your interest. TPack is a research project; contributions that help reproduce paper results, harden the OTel collector components for production, or extend the model are all welcome.

## Setup

```bash
git clone https://github.com/ProjectASAP/TPack.git
cd TPack

# Go (1.25+) — verify the workspace builds
go work sync && make build

# Python (3.13+) — install evaluation tools
uv pip install -e .
uv run scorecard --help

# Optional: run the docker demo
cd examples/basic && docker compose up --build
```

See [`docs/AE.md`](docs/AE.md#toolchain) for a full environment setup checklist (toolchain versions, OS-specific install commands).

## Running tests

```bash
make test                  # all Go modules
uv run pytest tpack_eval/  # Python tests
```

## Code style

- **Go**: `go fmt ./...` and `go vet ./...` (run via `make tidy`).
- **Python**: `uv run ruff check tpack_eval/` and `uv run ruff format tpack_eval/`.
- **Proto**: hand-edit `pkg/tpackmodel/proto/tpack.proto`, then `make proto-gen`.

## License

By contributing you agree your contributions are licensed under MIT (see `LICENSE`).
