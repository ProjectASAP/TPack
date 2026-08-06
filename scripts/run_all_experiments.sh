#!/usr/bin/env bash
set -euo pipefail

export PATH="$HOME/.local/bin:/usr/local/go/bin:$PATH"

usage() {
  echo "Usage: $0 [PART...]"
  echo "  Parts: otel-demo, feat-ablation, node-ablation, graph-ablation, root-duration-ablation, bounds-ablation, re2, uber, uber-scalability, strawman, figures, all (default: all)"
  echo "  Example: $0 otel-demo uber"
  exit 1
}

# Parse arguments
PARTS=("${@:-all}")
run_part() {
  local part="$1"
  for p in "${PARTS[@]}"; do
    [[ "$p" == "all" || "$p" == "$part" ]] && return 0
  done
  return 1
}

for p in "${PARTS[@]}"; do
  case "$p" in
    all|otel-demo|feat-ablation|node-ablation|graph-ablation|root-duration-ablation|bounds-ablation|re2|uber|uber-scalability|strawman|figures) ;;
    -h|--help) usage ;;
    *) echo "Unknown part: $p"; usage ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# Override with `DATA_DIR=/path/to/data bash scripts/run_all_experiments.sh ...`
# Defaults are relative to the TPack repo root.
DATA_DIR="${DATA_DIR:-data}"
OUTPUT_DIR="${OUTPUT_DIR:-output}"
CONFIG_DIR="configs"   # always shipped inside the repo

echo "Paths:"
echo "  DATA_DIR=$DATA_DIR"
echo "  OUTPUT_DIR=$OUTPUT_DIR"
echo "  CONFIG_DIR=$CONFIG_DIR"

# Build binary once upfront (avoids repeated `go run` compilation)
TPACK_EVAL="$ROOT/tpack-eval"
echo "Building tpack-eval..."
go build -o "$TPACK_EVAL" ./cmd/tpack-eval/

# Seeds for TPack runs (seed → run number). The paper averages over three seeds
# and reports standard deviations below 1% on every fidelity metric, so a single
# seed lands within ~1% of the published means. Reviewers who want the fast path
# can set NUM_SEEDS=1 and cut the TPack, head-sampling and Sifter work by ~3x at
# the cost of losing the error bars.
NUM_SEEDS="${NUM_SEEDS:-3}"
ALL_SEEDS=("42 1" "43 2" "44 3")
SEEDS=("${ALL_SEEDS[@]:0:$NUM_SEEDS}")

# Iterations for the sampling baselines (head, Sifter). Kept in lockstep with
# NUM_SEEDS so every approach gets the same number of repetitions and the cost
# comparison stays apples-to-apples.
ITERATIONS="${ITERATIONS:-$NUM_SEEDS}"

# Largest Uber size to run, in traces. Uber is remapped into a single 60-second
# bucket at roughly 950 spans per trace, so N is effectively spans-per-minute:
# 200000 traces is the ~187M spans/min figure the paper reports as the sustained
# single-collector rate. Capping here therefore still reproduces that claim while
# skipping the 500K point, which is by far the most expensive in the sweep.
UBER_MAX_TRACES="${UBER_MAX_TRACES:-500000}"

echo "  NUM_SEEDS=$NUM_SEEDS  ITERATIONS=$ITERATIONS  UBER_MAX_TRACES=$UBER_MAX_TRACES"

# Check if a compressor directory has all evaluation files (8 base + anomaly_detection).
is_done() {
  local dir="$1/evaluated"
  [ -d "$dir" ] && [ "$(ls "$dir" 2>/dev/null | wc -l)" -ge 9 ]
}

run_head_sampling() {
  local input="$1"
  local output_base="$2"
  local config="$3"
  local inject_time="${4:-}"  # optional path to inject_time.txt
  local rates_csv="${5:-1,2,3,4,5,10,20,50,100,500}"  # override for memory-tight runs
  local inject_flag=""
  [ -n "$inject_time" ] && inject_flag="--inject-time $inject_time"

  local rates
  IFS=',' read -ra rates <<< "$rates_csv"

  # Step 1: Sample (skip if all datasets exist)
  local SAMPLE_DONE=true
  for rate in "${rates[@]}"; do
    for iter in 1 2 3; do
      if [ ! -d "$output_base/head_${rate}_${iter}/dataset" ]; then
        SAMPLE_DONE=false
        break 2
      fi
    done
  done

  if $SAMPLE_DONE; then
    echo "  SKIP head sampling (all sampled)"
  else
    echo "  Running head sampling (rates: $rates_csv)..."
    "$TPACK_EVAL" \
      --config "$config" \
      --input "$input" \
      --output "$output_base" \
      --head-sample --sampling-rates "$rates_csv" --iterations "$ITERATIONS"
  fi

  # Step 2: Queue evaluations for parallel execution
  for rate in "${rates[@]}"; do
    for iter in 1 2 3; do
      local ds="$output_base/head_${rate}_${iter}/dataset"
      if [ -d "$ds" ] && ! is_done "$output_base/head_${rate}_${iter}"; then
        "$TPACK_EVAL" --evaluate-only \
          --config "$config" \
          --input "$ds" --output "$output_base/head_${rate}_${iter}/evaluated" $inject_flag
      fi
    done
  done
}

run_tail_sampling() {
  local input="$1"
  local output_base="$2"
  local config="$3"
  local inject_time="${4:-}"
  local inject_flag=""
  [ -n "$inject_time" ] && inject_flag="--inject-time $inject_time"

  if [ ! -d "$output_base/tail/dataset" ]; then
    echo "  Running tail sampling..."
    "$TPACK_EVAL" \
      --config "$config" \
      --input "$input" \
      --output "$output_base" \
      --tail-sample
  else
    echo "  SKIP tail sampling (already sampled)"
  fi

  if [ -d "$output_base/tail/dataset" ] && ! is_done "$output_base/tail"; then
    "$TPACK_EVAL" --evaluate-only \
      --config "$config" \
      --input "$output_base/tail/dataset" \
      --output "$output_base/tail/evaluated" $inject_flag
  fi
}

run_hindsight_sampling() {
  local input="$1"
  local output_base="$2"
  local config="$3"
  local inject_time="${4:-}"
  local inject_flag=""
  [ -n "$inject_time" ] && inject_flag="--inject-time $inject_time"

  if [ ! -d "$output_base/hindsight/dataset" ]; then
    echo "  Running hindsight sampling..."
    "$TPACK_EVAL" \
      --config "$config" \
      --input "$input" \
      --output "$output_base" \
      --hindsight-sample
  else
    echo "  SKIP hindsight sampling (already sampled)"
  fi

  if [ -d "$output_base/hindsight/dataset" ] && ! is_done "$output_base/hindsight"; then
    "$TPACK_EVAL" --evaluate-only \
      --config "$config" \
      --input "$output_base/hindsight/dataset" \
      --output "$output_base/hindsight/evaluated" $inject_flag
  fi
}

run_sifter_sampling() {
  local input="$1"
  local output_base="$2"
  local config="$3"
  local inject_time="${4:-}"
  local inject_flag=""
  [ -n "$inject_time" ] && inject_flag="--inject-time $inject_time"

  # Step 1: Sample (skip if all datasets exist)
  local SAMPLE_DONE=true
  for iter in 1 2 3; do
    if [ ! -d "$output_base/sifter_100_${iter}/dataset" ]; then
      SAMPLE_DONE=false
      break
    fi
  done

  if $SAMPLE_DONE; then
    echo "  SKIP sifter sampling (all sampled)"
  else
    echo "  Running sifter sampling..."
    "$TPACK_EVAL" \
      --config "$config" \
      --input "$input" \
      --output "$output_base" \
      --sifter-sample --sampling-rates 100 --iterations "$ITERATIONS" --seed 42
  fi

  # Step 2: Queue evaluations for parallel execution
  for iter in 1 2 3; do
    local ds="$output_base/sifter_100_${iter}/dataset"
    if [ -d "$ds" ] && ! is_done "$output_base/sifter_100_${iter}"; then
      "$TPACK_EVAL" --evaluate-only \
        --config "$config" \
        --input "$ds" --output "$output_base/sifter_100_${iter}/evaluated" $inject_flag
    fi
  done
}

run_tpack() {
  local input="$1"
  local output_base="$2"
  local config="$3"
  local seed="$4"
  local run="$5"
  local inject_time="${6:-}"
  local name="${7:-tpack}"
  local inject_flag=""
  [ -n "$inject_time" ] && inject_flag="--inject-time $inject_time"

  local dir="$output_base/${name}_${run}"

  if [ ! -d "$dir/dataset" ]; then
    echo "  Running ${name} run $run (seed $seed)..."
    "$TPACK_EVAL" \
      --config "$config" \
      --input "$input" \
      --output "$dir/dataset" \
      --seed "$seed"
  else
    echo "  SKIP ${name}_${run} compression (already done)"
  fi

  if [ -d "$dir/dataset" ] && ! is_done "$dir"; then
    "$TPACK_EVAL" --evaluate-only \
      --config "$config" \
      --input "$dir/dataset" \
      --output "$dir/evaluated" $inject_flag
  fi
}

# Symlink head_1_1 baseline from a source output dir into the target dir.
# Needed for ablation output dirs that skip head sampling — the report generator
# loads <target>/head_1_1/evaluated/*.json to compute fidelity deltas.
link_head_baseline() {
  local source_dir="$1"
  local target_dir="$2"
  if [ ! -d "$source_dir/head_1_1" ]; then
    echo "  WARN: baseline source $source_dir/head_1_1 not found — cannot symlink"
    return
  fi
  if [ -e "$target_dir/head_1_1" ]; then
    return  # already present
  fi
  mkdir -p "$target_dir"
  ln -sfn "$(cd "$source_dir/head_1_1" && pwd)" "$target_dir/head_1_1"
}

run_report() {
  local output_base="$1"
  local compressors="${2:-head,tpack}"
  local filename="${3:-report.json}"

  if [ -f "$output_base/$filename" ]; then
    echo "  SKIP $filename (already done)"
    return
  fi
  echo "  Generating $filename..."
  "$TPACK_EVAL" \
    --report \
    --output "$output_base/$filename" \
    --compressors "$compressors"
}

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1: otel-demo
# ═══════════════════════════════════════════════════════════════════════════════

OTEL_RAW="$DATA_DIR/otel-demo"
OTEL_INPUT="$DATA_DIR/otel-demo/transformed"
OTEL_OUTPUT="$OUTPUT_DIR/otel-demo"
OTEL_CONFIG="$CONFIG_DIR/otel_demo.yaml"

# One-shot transform; safe to call from any part.
ensure_otel_transformed() {
  if [ ! -d "$OTEL_INPUT" ] || [ -z "$(ls -A "$OTEL_INPUT" 2>/dev/null)" ]; then
    echo "  Transforming otel-demo → OTLP JSON..."
    "$TPACK_EVAL" --transform \
      --config "$OTEL_CONFIG" \
      --input "$OTEL_RAW" \
      --output "$OTEL_INPUT"
  else
    echo "  SKIP transform (already done)"
  fi
}

# Uber globals (shared across uber, uber-eval, graph/root-dur/child-dur ablations).
UBER_RAW="$DATA_DIR/uber-trace1"
UBER_CONFIG="$CONFIG_DIR/uber.yaml"
UBER_SCALABILITY_DATA="$DATA_DIR/uber-scalability"
UBER_SCALABILITY_OUTPUT="$OUTPUT_DIR/uber-scalability"

# Transform a specific uber size (remapped to a single 60s bucket). Idempotent.
ensure_uber_transformed() {
  local N="$1"
  local data_dir="$UBER_SCALABILITY_DATA/$N"
  if [ ! -d "$data_dir" ] || [ -z "$(ls -A "$data_dir" 2>/dev/null)" ]; then
    echo "  Transforming Uber → OTLP (${N} traces, 60s window, 100K spans/chunk)..."
    "$TPACK_EVAL" --transform \
      --config "$UBER_CONFIG" \
      --input "$UBER_RAW" \
      --output "$data_dir" \
      --remap \
      --max-traces "$N" \
      --max-spans-per-chunk 100000
  fi
}

# Run head 1:1 (identity sampling) in an isolated output dir to provide
# the fidelity baseline for downstream reports. Called from ablation parts
# when the main otel-demo head_1_1 isn't available.
ensure_head_1_1() {
  local input="$1"
  local output_base="$2"
  local config="$3"
  if [ -d "$output_base/head_1_1/evaluated" ] && is_done "$output_base/head_1_1"; then
    return
  fi
  if [ ! -d "$output_base/head_1_1/dataset" ]; then
    echo "  Running head 1:1 (baseline) in $output_base..."
    "$TPACK_EVAL" --config "$config" --input "$input" --output "$output_base" \
      --head-sample --sampling-rates 1 --iterations 1
  fi
  if [ -d "$output_base/head_1_1/dataset" ] && ! is_done "$output_base/head_1_1"; then
    "$TPACK_EVAL" --evaluate-only --config "$config" \
      --input "$output_base/head_1_1/dataset" \
      --output "$output_base/head_1_1/evaluated"
  fi
}

if run_part otel-demo; then
echo "═══ Part 1: otel-demo ═══"

ensure_otel_transformed

run_head_sampling "$OTEL_INPUT" "$OTEL_OUTPUT" "$OTEL_CONFIG"
run_tail_sampling "$OTEL_INPUT" "$OTEL_OUTPUT" "$OTEL_CONFIG"
run_hindsight_sampling "$OTEL_INPUT" "$OTEL_OUTPUT" "$OTEL_CONFIG"
run_sifter_sampling "$OTEL_INPUT" "$OTEL_OUTPUT" "$OTEL_CONFIG"

for seed_run in "${SEEDS[@]}"; do
  read -r seed run <<< "$seed_run"
  run_tpack "$OTEL_INPUT" "$OTEL_OUTPUT" "$OTEL_CONFIG" "$seed" "$run" "" tpack_default
done

run_report "$OTEL_OUTPUT" "head,tpack_default,tail,hindsight,sifter"

# Scorecard
cd "$ROOT"
echo "  Generating scorecard..."
uv run scorecard \
  --input "$OTEL_OUTPUT/report.json" \
  --approaches tpack_default
# (TPack root is workspace root; no cd needed)
fi # otel-demo

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1c: Feature ablation on otel-demo
# ═══════════════════════════════════════════════════════════════════════════════

if run_part feat-ablation; then
echo ""
echo "═══ Part 1c: Feature Ablation ═══"

# Self-contained: transform otel-demo if not yet done
ensure_otel_transformed

FEAT_OUTPUT="$OUTPUT_DIR/otel-demo-feat-ablation/1"

# Baseline: symlink head_1_1 from the main otel-demo output so fidelity deltas
# resolve against the same unsampled baseline.
link_head_baseline "$OTEL_OUTPUT" "$FEAT_OUTPUT"

# Feature column counts: 24, 23, 17, 12, 9, 7, 4
# feat22 omitted because otel_demo.yaml == the default config already run.
FEAT_CONFIGS=(
  "otel_demo_24   feat24"
  "otel_demo_23   feat23"
  "otel_demo_17   feat17"
  "otel_demo_12   feat12"
  "otel_demo_9    feat9"
  "otel_demo_7    feat7"
  "otel_demo_4    feat4"
)

for entry in "${FEAT_CONFIGS[@]}"; do
  read -r cfg name <<< "$entry"
  for seed_run in "${SEEDS[@]}"; do
    read -r seed run <<< "$seed_run"
    dir_name="tpack_${name}_${run}"
    if ! [ -d "$FEAT_OUTPUT/${dir_name}/dataset" ]; then
      echo "  Running feature ablation: $name (seed $seed)..."
      "$TPACK_EVAL" \
        --config "$CONFIG_DIR/${cfg}.yaml" \
        --input "$OTEL_INPUT" \
        --output "$FEAT_OUTPUT/${dir_name}/dataset" \
        --seed "$seed"
    fi
    if ! is_done "$FEAT_OUTPUT/$dir_name"; then
      "$TPACK_EVAL" --evaluate-only \
        --config "$CONFIG_DIR/${cfg}.yaml" \
        --input "$FEAT_OUTPUT/${dir_name}/dataset" \
        --output "$FEAT_OUTPUT/${dir_name}/evaluated"
    fi
  done
done

# Generate separate report for feature ablation (only feat variants)
run_report "$FEAT_OUTPUT" "tpack_feat4,tpack_feat7,tpack_feat9,tpack_feat12,tpack_feat17,tpack_feat23,tpack_feat24" "report_feat.json"

fi # feat-ablation

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1b: Strawman TVAE on otel-demo
# ═══════════════════════════════════════════════════════════════════════════════

if run_part strawman; then
echo ""
echo "═══ Part 1b: Strawman TVAE ═══"

ensure_otel_transformed

STRAW_CSV="$ROOT/$OTEL_INPUT/spans_flat.csv"
STRAW_DIR="$ROOT/$OTEL_OUTPUT/tvae_train_1"

if is_done "$STRAW_DIR"; then
  echo "  SKIP tvae_train (already done)"
else
  # Step 1: Flatten OTLP → CSV
  if [ ! -f "$STRAW_CSV" ]; then
    echo "  Flattening OTLP → CSV..."
    "$TPACK_EVAL" --flatten-csv \
      --config "$OTEL_CONFIG" \
      --input "$ROOT/$OTEL_INPUT" \
      --output "$STRAW_CSV"
  else
    echo "  SKIP flatten (CSV exists)"
  fi

  # Step 2: Train + Generate per bucket (Python, writes timing to compressed/data/)
  echo "  Training TVAE + generating per bucket..."
  cd "$ROOT"
  PYTHONUNBUFFERED=1 uv run tvae_train \
    --input "$STRAW_CSV" \
    --output "$STRAW_DIR/" \
    --seed 42 --epochs 20 --device cuda

  # Step 3: Reconstruct traces → OTLP JSON
  echo "  Reconstructing traces..."
  PYTHONUNBUFFERED=1 uv run tvae_reconstruct \
    --input "$STRAW_CSV" \
    --generated "$STRAW_DIR/" \
    --output "$STRAW_DIR/dataset"
  # (TPack root is workspace root; no cd needed)

  # Step 4: Evaluate
  echo "  Evaluating strawman output..."
  "$TPACK_EVAL" --evaluate-only \
    --config "$OTEL_CONFIG" \
    --input "$STRAW_DIR/dataset" \
    --output "$STRAW_DIR/evaluated"
fi

# Generate separate report for strawman
run_report "$OTEL_OUTPUT" "tvae_train" "report_strawman.json"

fi # strawman

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1d: Node ablation (leave-one-out feature removal on otel-demo) — fig12
# ═══════════════════════════════════════════════════════════════════════════════

if run_part node-ablation; then
echo ""
echo "═══ Part 1d: Node Ablation (leave-one-out feature removal) ═══"

NODE_INPUT="$OTEL_INPUT"
NODE_OUTPUT="$OUTPUT_DIR/otel-demo-node-ablation/1"

# Self-contained: run transform if missing so this part is individually invocable.
ensure_otel_transformed

# Generate leave-one-out configs if not already present
if [ ! -f "$ROOT/configs/ablation/otel_demo_no_service_name.yaml" ]; then
  echo "  Generating feature ablation configs..."
  cd "$ROOT"
  uv run generate_feature_ablation_configs
  # (TPack root is workspace root; no cd needed)
fi

# head_1_1 baseline: prefer symlink from the main otel-demo output if present,
# otherwise generate our own (keeps node-ablation self-contained).
if [ -d "$OTEL_OUTPUT/head_1_1/evaluated" ]; then
  link_head_baseline "$OTEL_OUTPUT" "$NODE_OUTPUT"
else
  ensure_head_1_1 "$NODE_INPUT" "$NODE_OUTPUT" "$OTEL_CONFIG"
fi

# Baseline: full TPack with default config (1 seed)
run_tpack "$NODE_INPUT" "$NODE_OUTPUT" "$OTEL_CONFIG" 42 1 "" tpack_default

# Leave-one-out variants (1 seed each)
for cfg_file in "$ROOT"/configs/ablation/otel_demo_no_*.yaml; do
  cfg_name=$(basename "$cfg_file" .yaml)
  short_name="${cfg_name#otel_demo_}"  # no_service_name, no_http_url, ...
  run_tpack "$NODE_INPUT" "$NODE_OUTPUT" "$cfg_file" 42 1 "" "tpack_${short_name}"
done

# Add-one-in variant: net.peer.port promoted from metadata to feature
run_tpack "$NODE_INPUT" "$NODE_OUTPUT" \
  "$CONFIG_DIR/ablation/otel_demo_add_net_peer_port.yaml" \
  42 1 "" tpack_add_net_peer_port

# Assemble report_node.json compressor list
NODE_COMPRESSORS="tpack_default,tpack_add_net_peer_port"
for cfg_file in "$ROOT"/configs/ablation/otel_demo_no_*.yaml; do
  cfg_name=$(basename "$cfg_file" .yaml)
  short_name="${cfg_name#otel_demo_}"
  NODE_COMPRESSORS="${NODE_COMPRESSORS},tpack_${short_name}"
done
run_report "$NODE_OUTPUT" "$NODE_COMPRESSORS" "report_node.json"

fi # node-ablation

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1e: Graph ablation (aggregation-only, reads from `uber` part outputs)
# ═══════════════════════════════════════════════════════════════════════════════

if run_part graph-ablation; then
echo ""
echo "═══ Part 1e: Graph Ablation (aggregate from uber) ═══"

# Requires the `uber` part to have been run first so that
# output/uber/{N}/report.json contains tpack_default + tpack_template compressors
# for each size N, and data/uber-scalability/{N}/stats.json has template/edge
# counts for annotations.
cd "$ROOT"
uv run collect_graph_ablation \
  --data-root "$UBER_SCALABILITY_DATA" \
  --output-root "$OUTPUT_DIR/uber" \
  --sizes 1000,2000,5000,10000,20000,50000 \
  --out data/paper/fig13_graph_ablation.json
# (TPack root is workspace root; no cd needed)

fi # graph-ablation

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1f: Root-duration ablation (GMM K=1..5, uber 20k) — fig14
# ═══════════════════════════════════════════════════════════════════════════════

if run_part root-duration-ablation; then
echo ""
echo "═══ Part 1f: Root-duration Ablation (GMM K=1..5, otel-demo) ═══"

RD_DATA_DIR="$OTEL_INPUT"
RD_OUTPUT_DIR="$OUTPUT_DIR/otel-demo-gmm-ablation/1"

ensure_otel_transformed

if [ -d "$OTEL_OUTPUT/head_1_1/evaluated" ]; then
  link_head_baseline "$OTEL_OUTPUT" "$RD_OUTPUT_DIR"
else
  ensure_head_1_1 "$RD_DATA_DIR" "$RD_OUTPUT_DIR" "$OTEL_CONFIG"
fi

for K in 1 2 3 4 5; do
  CFG="$CONFIG_DIR/ablation/otel_demo_gmm${K}.yaml"
  run_tpack "$RD_DATA_DIR" "$RD_OUTPUT_DIR" "$CFG" 42 1 "" "tpack_gmm${K}"
done
run_report "$RD_OUTPUT_DIR" "tpack_gmm1,tpack_gmm2,tpack_gmm3,tpack_gmm4,tpack_gmm5" "report_gmm.json"

fi # root-duration-ablation

# ═══════════════════════════════════════════════════════════════════════════════
# Part 1h: Bounds ablation (with vs without gap/duration clamping, otel-demo template) — fig16
# ═══════════════════════════════════════════════════════════════════════════════

if run_part bounds-ablation; then
echo ""
echo "═══ Part 1h: Bounds Ablation (with vs without, otel-demo template) ═══"

BD_DATA_DIR="$OTEL_INPUT"
BD_OUTPUT_DIR="$OUTPUT_DIR/otel-demo-bounds-ablation/1"

ensure_otel_transformed

if [ -d "$OTEL_OUTPUT/head_1_1/evaluated" ]; then
  link_head_baseline "$OTEL_OUTPUT" "$BD_OUTPUT_DIR"
else
  ensure_head_1_1 "$BD_DATA_DIR" "$BD_OUTPUT_DIR" "$OTEL_CONFIG"
fi

for seed_run in "${SEEDS[@]}"; do
  read -r seed run <<< "$seed_run"
  run_tpack "$BD_DATA_DIR" "$BD_OUTPUT_DIR" "$CONFIG_DIR/ablation/otel_demo_bounds.yaml"   "$seed" "$run" "" tpack_bounds
  run_tpack "$BD_DATA_DIR" "$BD_OUTPUT_DIR" "$CONFIG_DIR/ablation/otel_demo_nobounds.yaml" "$seed" "$run" "" tpack_nobounds
done
run_report "$BD_OUTPUT_DIR" "tpack_bounds,tpack_nobounds" "report_bounds.json"

fi # bounds-ablation

# ═══════════════════════════════════════════════════════════════════════════════
# Part 2 & 3: RE2-TT and RE2-OB
# ═══════════════════════════════════════════════════════════════════════════════

RE2_CONFIG="$CONFIG_DIR/re2.yaml"

if run_part re2; then
for dataset in RE2-TT RE2-OB; do
  echo ""
  echo "═══ Part: $dataset ═══"

  # Derive per-dataset paths into fresh variables. Assigning back into
  # DATA_DIR/OUTPUT_DIR here compounded them across loop iterations, so the
  # RE2-OB pass looked under <data>/RE2/RE2-TT/RE2/RE2-OB, matched nothing, and
  # silently produced no output at all.
  DATASET_DATA_DIR="$DATA_DIR/RE2/$dataset"
  DATASET_OUTPUT_DIR="$OUTPUT_DIR/RE2/$dataset"

  if [ ! -d "$DATASET_DATA_DIR" ]; then
    echo "  ERROR: $DATASET_DATA_DIR not found — run scripts/download_data.sh re2" >&2
    exit 1
  fi

  for service_dir in "$DATASET_DATA_DIR"/*/; do
    service=$(basename "$service_dir")

    for run_num in 1 2 3; do
      csv="$service_dir/$run_num/traces.csv"
      [ -f "$csv" ] || continue

      input="$service_dir/$run_num/traces-transformed"
      if [ ! -d "$input" ] || [ -z "$(ls -A "$input" 2>/dev/null)" ]; then
        echo "  Transforming CSV → OTLP JSON for $service run $run_num..."
        "$TPACK_EVAL" --transform --config "$RE2_CONFIG" --input "$csv" --output "$input"
      fi

      output_base="$DATASET_OUTPUT_DIR/$service/$run_num"
      echo "  --- $service/$run_num ---"

      # Pass inject_time.txt if it exists (RE2 fault-injection datasets)
      inject_time_file="$service_dir/$run_num/inject_time.txt"
      inject_time_arg=""
      [ -f "$inject_time_file" ] && inject_time_arg="$inject_time_file"

      run_head_sampling "$input" "$output_base" "$RE2_CONFIG" "$inject_time_arg"
      run_tail_sampling "$input" "$output_base" "$RE2_CONFIG" "$inject_time_arg"
      run_hindsight_sampling "$input" "$output_base" "$RE2_CONFIG" "$inject_time_arg"
      run_sifter_sampling "$input" "$output_base" "$RE2_CONFIG" "$inject_time_arg"
      for seed_run in "${SEEDS[@]}"; do
        read -r seed run <<< "$seed_run"
        run_tpack "$input" "$output_base" "$RE2_CONFIG" "$seed" "$run" "$inject_time_arg" tpack_default
      done
      run_report "$output_base" "head,tpack_default,tail,hindsight,sifter"
    done
  done

  # Aggregate scorecard across all scenarios and runs.
  # Use $DATASET_OUTPUT_DIR, not a hardcoded relative output/ path, so this
  # still finds the reports when OUTPUT_DIR points somewhere else.
  cd "$ROOT"
  echo "  Generating aggregate scorecard for $dataset..."
  uv run scorecard \
    --input "$DATASET_OUTPUT_DIR"/*/*/report.json \
    --approaches tpack_default
done
fi # re2

# ═══════════════════════════════════════════════════════════════════════════════
# Part 4: Uber Scalability (throughput sweep, no eval)
# ═══════════════════════════════════════════════════════════════════════════════

if run_part uber-scalability; then
echo ""
echo "═══ Part 4: Uber Scalability ═══"

for N in 10000 20000 50000 100000 200000 500000; do
  if [ "$N" -gt "$UBER_MAX_TRACES" ]; then
    echo "  SKIP N=$N (above UBER_MAX_TRACES=$UBER_MAX_TRACES)"
    continue
  fi
  echo "  --- ${N} traces ---"
  # Loop-local names: assigning DATA_DIR/OUTPUT_DIR here would clobber the
  # globals for every later part (the `uber` part would then write under
  # $OUTPUT_DIR/uber-scalability/<last N>/uber/...).
  SCAL_DATA="$UBER_SCALABILITY_DATA/$N"
  SCAL_OUTPUT="$UBER_SCALABILITY_OUTPUT/$N"

  ensure_uber_transformed "$N"

  # Compress (1 seed)
  DS="$SCAL_OUTPUT/tpack_1/dataset"
  COMPRESSED="$SCAL_OUTPUT/tpack_1/compressed/data"
  if [ -d "$COMPRESSED" ] && [ -f "$COMPRESSED/compression_cpu_time_seconds" ]; then
    echo "  SKIP tpack_1 (already done)"
  else
    echo "  Running tpack run 1 (seed 42)..."
    "$TPACK_EVAL" \
      --config "$UBER_CONFIG" \
      --input "$SCAL_DATA" \
      --output "$DS" \
      --seed 42 \
      --skip-output
  fi

  # No evaluation for scalability (--skip-output means no OTLP files)
done

# Collect scalability results into JSON for plotting.
# Pass $UBER_SCALABILITY_{DATA,OUTPUT} rather than literal data//output/ paths:
# with a custom DATA_DIR/OUTPUT_DIR the literals resolve under $ROOT, where
# nothing was written, and collect_scalability then writes an empty JSON and
# exits 0 — a silent failure. Relative defaults still resolve correctly here
# because we cd to $ROOT first.
echo "  Collecting scalability results..."
cd "$ROOT"
uv run collect_scalability \
  --data-dir "$UBER_SCALABILITY_DATA" \
  --output-dir "$UBER_SCALABILITY_OUTPUT" \
  --out data/paper/fig11_scalability.json
# (TPack root is workspace root; no cd needed)
fi # uber-scalability

# ═══════════════════════════════════════════════════════════════════════════════
# Part 4b: Uber Fidelity Evaluation (sizes 1K-50K)
# Also produces tpack_template runs so graph-ablation can aggregate without
# duplicated compute; dataset-stats (including unique_edges) are cached in
# data/uber-scalability/{N}/stats.json.
# ═══════════════════════════════════════════════════════════════════════════════

if run_part uber; then
echo ""
echo "═══ Part 4b: Uber Fidelity Evaluation (sizes 1K-50K) ═══"

UBER_TEMPLATE_CONFIG="$CONFIG_DIR/ablation/uber_template.yaml"
UBER_SIZES=()
for _n in 1000 2000 5000 10000 20000 50000; do
  [ "$_n" -le "$UBER_MAX_TRACES" ] && UBER_SIZES+=("$_n")
done
if [ ${#UBER_SIZES[@]} -eq 0 ]; then
  echo "  ERROR: UBER_MAX_TRACES=$UBER_MAX_TRACES is below the smallest size (1000)" >&2
  exit 1
fi

for N in "${UBER_SIZES[@]}"; do
  echo ""
  echo "  --- Uber N=$N ---"
  UBER_INPUT="$UBER_SCALABILITY_DATA/$N"
  UBER_OUTPUT="$OUTPUT_DIR/uber/$N"

  ensure_uber_transformed "$N"

  # Rate 1 now uses hardlinks (head_sampling.go), so head sampling across all
  # rates × 3 iterations is memory-safe even at N=50K.
  run_head_sampling      "$UBER_INPUT" "$UBER_OUTPUT" "$UBER_CONFIG"
  run_tail_sampling      "$UBER_INPUT" "$UBER_OUTPUT" "$UBER_CONFIG"
  run_hindsight_sampling "$UBER_INPUT" "$UBER_OUTPUT" "$UBER_CONFIG"
  # Sifter disabled for Uber: too slow on large vocab (~60+ min per iteration).

  for seed_run in "${SEEDS[@]}"; do
    read -r seed run <<< "$seed_run"
    run_tpack "$UBER_INPUT" "$UBER_OUTPUT" "$UBER_CONFIG"          "$seed" "$run" "" tpack_default
    run_tpack "$UBER_INPUT" "$UBER_OUTPUT" "$UBER_TEMPLATE_CONFIG" "$seed" "$run" "" tpack_template
  done

  # Cache dataset-stats (unique_templates + unique_edges) for fig13 annotations.
  if [ ! -f "$UBER_INPUT/stats.json" ]; then
    echo "  Computing dataset-stats (basic,templates) for N=$N..."
    "$TPACK_EVAL" --dataset-stats --stats-sections basic,templates \
      --config "$UBER_CONFIG" --input "$UBER_INPUT" \
      --stats-output-json "$UBER_INPUT/stats.json"
  fi

  run_report "$UBER_OUTPUT" "head,tpack_default,tpack_template,tail,hindsight"
done

# Aggregate scorecard at the largest size that was actually run.
cd "$ROOT"
UBER_TOP_N="${UBER_SIZES[${#UBER_SIZES[@]}-1]}"
echo "  Generating scorecard at N=$UBER_TOP_N..."
uv run scorecard \
  --input "$OUTPUT_DIR/uber/$UBER_TOP_N/report.json" \
  --approaches tpack_default
# (TPack root is workspace root; no cd needed)
fi # uber

# ═══════════════════════════════════════════════════════════════════════════════
# Part 5: Paper Figures
# ═══════════════════════════════════════════════════════════════════════════════

if run_part figures; then
echo ""
echo "═══ Part 5: Paper Figures ═══"
cd "$ROOT"

OTEL_REPORT="$OTEL_OUTPUT/report.json"
RCA_REPORT="$OUTPUT_DIR/RE2/RE2-TT/ts-auth-service_cpu/1/report.json"

# Collect RE2 report paths (all error types, all runs)
RE2TT_REPORTS=$(echo "$OUTPUT_DIR"/RE2/RE2-TT/*/*/report.json | tr ' ' ',')
RE2OB_REPORTS=$(echo "$OUTPUT_DIR"/RE2/RE2-OB/*/*/report.json | tr ' ' ',')
UBER_REPORT="$OUTPUT_DIR/uber/50000/report.json"

OTEL_REPORT_FEAT="$OUTPUT_DIR/otel-demo-feat-ablation/1/report_feat.json"
PAPERDIR="output/paper-figures"

# Batch: report-based figures + data-dir figures/tables (cache avoids re-loading)
uv run plot_paper --mode query_fidelity scalability tab_scalability \
  --report "$OTEL_REPORT" --data-dir data/paper --paper-dir "$PAPERDIR"
uv run plot_paper --mode tradeoff --report "$OTEL_REPORT" "$OTEL_REPORT_FEAT" --paper-dir "$PAPERDIR"
uv run plot_paper --mode cost_fidelity tab_cross_dataset --paper-dir "$PAPERDIR" \
  --datasets "OTel Demo:$OTEL_REPORT" \
             "RE2-TT:$RE2TT_REPORTS" \
             "RE2-OB:$RE2OB_REPORTS" \
             "Uber (50K):$UBER_REPORT"
uv run plot_paper --mode rca --paper-dir "$PAPERDIR" \
  --datasets "RE2-TT:$RE2TT_REPORTS" \
             "RE2-OB:$RE2OB_REPORTS"

# Component ablations (node/graph active; root-duration/reject-sampling commented out of paper)
OTEL_REPORT_NODE="$OUTPUT_DIR/otel-demo-node-ablation/1/report_node.json"

[ -f "$OTEL_REPORT_NODE" ] && uv run plot_paper --mode node_ablation \
  --report "$OTEL_REPORT_NODE" "$OTEL_REPORT" --paper-dir "$PAPERDIR"
[ -f "data/paper/fig13_graph_ablation.json" ] && uv run plot_paper --mode graph_ablation \
  --data-dir data/paper --paper-dir "$PAPERDIR"

# (TPack root is workspace root; no cd needed)
fi # figures

rm -f "$TPACK_EVAL"

echo ""
echo "═══ All experiments complete ═══"
