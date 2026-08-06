#!/usr/bin/env bash
# Five-gate smoke test for TPack. Run from repo root: bash scripts/smoke_test.sh
#
# Exits non-zero on the first failing gate.

set -euo pipefail

cd "$(dirname "$0")/.."

echo "════════════════════════════════════════════════════════════"
echo " Gate 1: Go workspace builds + tests pass"
echo "════════════════════════════════════════════════════════════"
go work sync
# Regenerating the protobuf bindings requires protoc >= 3.21 and is only needed
# when pkg/tpackmodel/proto/*.proto changes. The generated *.pb.go files are
# committed, so the default path does not need protoc at all. Opt in with
# SMOKE_PROTO=1 if you have edited a .proto file.
if [ "${SMOKE_PROTO:-0}" = "1" ]; then
  make tidy
  make proto-gen
else
  echo "  (skipping proto-gen; set SMOKE_PROTO=1 to regenerate protobuf bindings)"
fi
make build
make test
echo "✓ Gate 1 passed"

echo
echo "════════════════════════════════════════════════════════════"
echo " Gate 2: tpack-eval binary built"
echo "════════════════════════════════════════════════════════════"
go build -o tpack-eval ./cmd/tpack-eval/
test -x ./tpack-eval
./tpack-eval --help >/dev/null 2>&1 || true   # tpack-eval prints usage to stderr by default
echo "✓ Gate 2 passed"

echo
echo "════════════════════════════════════════════════════════════"
echo " Gate 3: Python pkg installs and CLIs resolve"
echo "════════════════════════════════════════════════════════════"
uv sync --frozen >/dev/null
uv run --no-sync scorecard --help >/dev/null
uv run --no-sync plot_paper --help >/dev/null
uv run --no-sync tvae_train --help >/dev/null
uv run --no-sync tvae_reconstruct --help >/dev/null
echo "✓ Gate 3 passed"

echo
echo "════════════════════════════════════════════════════════════"
echo " Gate 4: no leaked legacy identifiers"
echo "════════════════════════════════════════════════════════════"
LEAKS=$(grep -rn "\bGenT\|\bGen-T\|\bgent_\|gent-eval\|otelcol-gent\|gentmodel\|gentexporter\|gentreceiver\|GenTEval\|genteval" \
  --include="*.go" --include="*.proto" --include="*.py" --include="*.yaml" --include="*.yml" --include="*.toml" --include="*.sh" --include="*.md" \
  . 2>/dev/null \
  | grep -v "/proto/.*\.pb\.go:" \
  | grep -v "examples/basic/tracegen/" \
  | grep -v "agent-side\|agentic" \
  | grep -v "^\./\.venv/" \
  | grep -v "^\./scripts/smoke_test\.sh:" \
  | grep -v "^\./CONTRIBUTING\.md:" \
  | grep -v "^\./CHANGELOG\.md:" \
  || true)
if [ -n "$LEAKS" ]; then
  echo "✗ Gate 4 FAILED — legacy identifiers found:"
  echo "$LEAKS"
  exit 1
fi
echo "✓ Gate 4 passed"

echo
echo "════════════════════════════════════════════════════════════"
echo " Gate 5: paper-terminology consistency"
echo "════════════════════════════════════════════════════════════"
TERM_LEAKS=$(grep -rn "\bfeature_columns\b\|\bmetadata_columns\b\|\bspan_type\b" \
  --include="*.go" --include="*.proto" --include="*.py" --include="*.yaml" --include="*.toml" --include="*.sh" \
  . 2>/dev/null \
  | grep -v "/proto/.*\.pb\.go:" \
  | grep -v "examples/basic/tracegen/" \
  | grep -v "^\./\.venv/" \
  | grep -v "^\./scripts/smoke_test\.sh:" \
  | grep -v "^\./CONTRIBUTING\.md:" \
  || true)
if [ -n "$TERM_LEAKS" ]; then
  echo "✗ Gate 5 FAILED — pre-paper-rename terminology found:"
  echo "$TERM_LEAKS"
  exit 1
fi
echo "✓ Gate 5 passed"

echo
echo "════════════════════════════════════════════════════════════"
echo " ALL GATES PASSED"
echo "════════════════════════════════════════════════════════════"
echo "TPack/ is ready. Next steps:"
echo "  • cd examples/basic && docker compose up --build      (end-to-end demo)"
echo "  • bash scripts/run_all_experiments.sh otel-demo       (paper main result)"
