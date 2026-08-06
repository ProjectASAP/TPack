#!/usr/bin/env bash
# Download datasets used by the TPack paper into $DATA_DIR (default: data/).
# Run from the TPack repo root: bash scripts/download_data.sh [DATASET ...]
#
# Datasets: otel-demo, re2, uber1. Default: all available.
#
# Override the destination with DATA_DIR=/path/to/data bash scripts/download_data.sh

set -euo pipefail

cd "$(dirname "$0")/.."

DATA_DIR="${DATA_DIR:-data}"
TMP_DIR="${TMP_DIR:-/tmp}"

# zenodo records (DOI → record ID)
OTEL_DEMO_URL="https://zenodo.org/records/20088885/files/otel-demo.tar.zst"
OTEL_DEMO_SHA256="927d7e5bfb1b017e72d65803c1e33ca0a5778017d52bf7cbb98315df7907c177"

# RE2 (RCAEval benchmark, DOI 10.5281/zenodo.14590730, CC BY 4.0).
# Two systems carry traces: Train Ticket (TT) and Online Boutique (OB).
# Sock Shop (RE2-SS) ships no traces, so it is not fetched.
RE2_TT_URL="https://zenodo.org/api/records/14590730/files/RE2-TT.zip/content"
RE2_TT_MD5="a7fbcd1ada406067dcc50771ae398408"
RE2_OB_URL="https://zenodo.org/api/records/14590730/files/RE2-OB.zip/content"
RE2_OB_MD5="b9e23f8842c404b396ffd2becff15de4"

# Uber trace1 ("uber1") — The Tale of Errors in Microservices, Artifact part 1
# (DOI 10.5281/zenodo.13947828, CC BY 4.0). Shipped as ~92 split parts
# (trace1_aa … trace1_dn) that concatenate into one zstd-compressed tar.
UBER_RECORD="13947828"

# Datasets to fetch (CLI args or default to all hosted).
DATASETS=("${@}")
if [ ${#DATASETS[@]} -eq 0 ]; then
  DATASETS=(otel-demo)
  [ -n "$RE2_TT_URL" ] && DATASETS+=(re2)
  [ -n "$UBER_RECORD" ] && DATASETS+=(uber1)
fi

for d in "${DATASETS[@]}"; do
  case "$d" in
    otel-demo|re2|uber1) ;;
    *) echo "Unknown dataset: $d (valid: otel-demo, re2, uber1)"; exit 1 ;;
  esac
done

# Tools
for tool in curl zstd tar sha256sum unzip md5sum; do
  command -v "$tool" >/dev/null || { echo "Required: $tool (not on PATH)"; exit 1; }
done

mkdir -p "$DATA_DIR" "$TMP_DIR"

fetch() {
  local name="$1"
  local url="$2"
  local sha256="$3"
  local tmp="$TMP_DIR/$name.tar.zst"

  if [ -z "$url" ]; then
    echo "  SKIP $name (not yet hosted — see docs/DATASETS.md)"
    return
  fi

  echo "═══ $name ═══"
  if [ -f "$tmp" ]; then
    echo "  Cached: $tmp"
  else
    echo "  Downloading from $url"
    curl -L -o "$tmp" "$url"
  fi

  echo "  Verifying SHA256..."
  echo "$sha256  $tmp" | sha256sum -c

  echo "  Extracting into $DATA_DIR/"
  zstd -d -c "$tmp" | tar -x -C "$DATA_DIR"
  echo "  ✓ $name"
}

# fetch_re2_zip downloads one RCAEval RE2 system zip, verifies its md5, and
# normalizes the extracted tree to the canonical layout the tooling expects:
#   $DATA_DIR/RE2/<system>/<service>_<fault>/<run>/traces.csv
#
# Only traces.csv and inject_time.txt are extracted — TPack does not use the
# logs/metrics files, which dominate the archive (RE2-TT decompresses to tens
# of GB if extracted in full). The zip's internal top-level folder name is not
# assumed — we locate the directory two levels above any traces.csv and
# relocate it as RE2/<system>. Staging happens on the destination filesystem
# (not $TMP_DIR, which may be a small tmpfs).
fetch_re2_zip() {
  local system="$1"   # RE2-TT or RE2-OB
  local url="$2"
  local md5="$3"
  local tmp="$TMP_DIR/$system.zip"
  local dest="$DATA_DIR/RE2/$system"

  echo "═══ $system ═══"
  if [ -d "$dest" ] && find "$dest" -maxdepth 3 -name traces.csv -print -quit | grep -q .; then
    echo "  Already present: $dest"
    return
  fi

  if [ -f "$tmp" ]; then
    echo "  Cached: $tmp"
  else
    echo "  Downloading from $url"
    curl -L -o "$tmp" "$url"
  fi

  echo "  Verifying MD5..."
  echo "$md5  $tmp" | md5sum -c

  local stage="$DATA_DIR/RE2/.$system-stage"
  rm -rf "$stage"
  mkdir -p "$stage"
  echo "  Unzipping traces.csv + inject_time.txt..."
  unzip -q "$tmp" '*/traces.csv' '*/inject_time.txt' -d "$stage"

  # Find where traces.csv lives, then take the dir two levels up
  # (<root>/<service>_<fault>/<run>/traces.csv → <root>).
  local sample root
  sample="$(find "$stage" -name traces.csv -print -quit)"
  if [ -z "$sample" ]; then
    echo "  ERROR: no traces.csv found inside $system.zip" >&2
    exit 1
  fi
  root="$(dirname "$(dirname "$(dirname "$sample")")")"

  echo "  Installing into $dest"
  rm -rf "$dest"
  mkdir -p "$(dirname "$dest")"
  mv "$root" "$dest"
  rm -rf "$stage"
  echo "  ✓ $system"
}

# fetch_uber downloads the Uber trace1 dataset from Zenodo. The archive is split
# into ~92 binary parts (trace1_aa … trace1_dn, ~21 GB total) that are pieces of
# a single zstd-compressed tar; concatenated in name order they decompress to a
# directory of Jaeger JSON traces (~300–500 GB — ensure that much free space).
#
# The per-part MD5s are read from the Zenodo manifest at runtime (84 checksums
# are too many to hardcode). Parts are staged on the destination filesystem (not
# $TMP_DIR, which may be a small tmpfs) and streamed through zstd|tar so the full
# tarball is never materialized.
fetch_uber() {
  local dest="$DATA_DIR/uber-trace1"
  local stage="$DATA_DIR/.uber-parts"

  echo "═══ uber1 (Zenodo $UBER_RECORD) ═══"
  if [ -d "$dest" ] && [ -n "$(ls -A "$dest" 2>/dev/null)" ]; then
    echo "  Already present: $dest"
    return
  fi
  command -v python3 >/dev/null || { echo "Required: python3 (for uber1 manifest)"; exit 1; }

  mkdir -p "$stage"
  echo "  Fetching manifest from Zenodo record $UBER_RECORD..."
  local manifest
  manifest="$(curl -fsSL "https://zenodo.org/api/records/$UBER_RECORD" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for f in sorted(d["files"], key=lambda x: x["key"]):
    if f["key"].startswith("trace1_"):
        print(f["key"] + "\t" + f["checksum"].split(":")[-1])')"
  [ -n "$manifest" ] || { echo "  ERROR: no trace1_* parts found in record $UBER_RECORD" >&2; exit 1; }

  local parts=()
  local name md5 f
  while IFS=$'\t' read -r name md5; do
    [ -n "$name" ] || continue
    f="$stage/$name"
    if [ ! -f "$f" ]; then
      echo "  Downloading $name"
      curl -fL -o "$f" "https://zenodo.org/api/records/$UBER_RECORD/files/$name/content"
    fi
    echo "$md5  $f" | md5sum -c
    parts+=("$f")
  done <<< "$manifest"

  echo "  Reassembling ${#parts[@]} parts + extracting into $dest (needs ~300–500 GB free)..."
  mkdir -p "$dest"
  # --strip-components=1 drops the archive's top-level traces-sanitized/ directory
  # so the .json files land directly in $dest, which is the layout docs/DATASETS.md
  # documents and the only one tpack-eval accepts: its input detection globs
  # "$input/*.json" without recursing, and a directory containing no .json but at
  # least one subdirectory is misdetected as an RE2 CSV tree rather than rejected.
  cat "${parts[@]}" | zstd -dc | tar -xf - -C "$dest" --strip-components=1
  rm -rf "$stage"
  echo "  ✓ uber1 → $dest"
}

for d in "${DATASETS[@]}"; do
  case "$d" in
    otel-demo)  fetch otel-demo "$OTEL_DEMO_URL" "$OTEL_DEMO_SHA256" ;;
    re2)        fetch_re2_zip RE2-TT "$RE2_TT_URL" "$RE2_TT_MD5"
                fetch_re2_zip RE2-OB "$RE2_OB_URL" "$RE2_OB_MD5" ;;
    uber1)      fetch_uber ;;
  esac
done

echo
echo "Done. Datasets installed under: $DATA_DIR"
