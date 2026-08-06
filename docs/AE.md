# Artifact Evaluation — NSDI '27

This document is the entry point for NSDI '27 artifact reviewers. It is
organized as the Call for Artifacts asks: a **Getting Started** section you can
finish in well under 30 minutes, followed by **Detailed Instructions** for a
full evaluation.

If you are not an AEC reviewer, you probably want [`../README.md`](../README.md).

## Badges claimed

We are submitting for **Artifacts Available**, **Artifacts Functional**, and
**Results Reproduced**.

Every experiment behind the paper's five Findings has been run end to end from
the published datasets, on hardware that is not the paper's workstation. The
[claims table](#the-claims) below maps each Finding to a command and a verdict,
and [`../results/reference/RESULTS.md`](../results/reference/RESULTS.md) puts our
measured value beside the paper's for each one.

## Where the artifact lives

| | |
|---|---|
| Source code | <https://github.com/ProjectASAP/TPack>, tag **`nsdi27-ae`** |
| otel-demo dataset | [10.5281/zenodo.20088885](https://doi.org/10.5281/zenodo.20088885) (ours, 63 MB) |
| RE2 dataset | [10.5281/zenodo.14590730](https://doi.org/10.5281/zenodo.14590730) (RCAEval, CC BY 4.0) |
| Uber dataset | [10.5281/zenodo.13947828](https://doi.org/10.5281/zenodo.13947828) (third party, CC BY 4.0) |

Please evaluate the **`nsdi27-ae`** tag. We may push bug fixes to it during the
kick-the-tires response period; we will not change anything else. `main` may
move ahead of the tag.

---

# Getting Started Instructions

Target: **under 30 minutes**, no datasets, no GPU. This is enough to tell
whether anything is obviously broken.

## 0. Prerequisites

Docker Engine 24+ with the Compose v2 plugin, and Go 1.25 if you want to run
the build gate. Nothing else. In particular **you do not need `protoc`** — the
generated protobuf bindings are committed.

## 1. Clone

```bash
git clone https://github.com/ProjectASAP/TPack.git
cd TPack
git checkout nsdi27-ae
```

## 2. End-to-end demo (~5 minutes)

This is the fastest way to see the whole idea working. It brings up a trace
generator, an edge collector that compresses traces into a model, a gRPC model
stream, a backend collector that regenerates traces from that model, and two
Grafana instances so you can compare.

```bash
cd examples/basic
docker compose up --build
```

Cold build takes 3–4 minutes. **About 60 seconds after the containers start** —
one flush interval — you should see in the logs:

```
otelcol-edge     | Model serialized              size_bytes=NNNNN
otelcol-backend  | Received model from server    size_bytes=NNNNN trace_count=NNN
otelcol-backend  | Generated traces              count=NNN total_spans=NNNN
```

One of our runs flushed 275 traces / 1,173 spans into a **51,641-byte** model,
and regenerated 275 traces / 1,204 spans from it. Your counts will differ —
`tracegen` runs continuously, so what lands in the first 60-second bucket depends
on when the containers came up — but the trace count in and out should match and
the span counts should be close.

Then confirm both backends hold traces:

```bash
curl -s localhost:3200/api/search | jq '.traces | length'   # original
curl -s localhost:3201/api/search | jq '.traces | length'   # TPack-regenerated
```

Both should be non-zero. Open <http://localhost:3001> (original) and
<http://localhost:3002> (TPack) — the rate / error / duration panels should have
a similar shape on both sides, which is the paper's core claim in miniature: the
right-hand dashboard was reconstructed from a model of a few kilobytes.

Tear down with `docker compose down -v`. Demo-specific troubleshooting is in
[`../examples/basic/README.md`](../examples/basic/README.md).

## 3. Build and test gate (~5 minutes)

```bash
cd ../..          # back to repo root
bash scripts/smoke_test.sh
```

Five gates: Go workspace builds, `tpack-eval` binary links and runs, the Python
package installs from the lockfile and its CLIs resolve, and two repo-hygiene
checks. All five should print `✓`.

**If steps 2 and 3 pass, kick-the-tires is satisfied.** Everything below is the
full evaluation.

---

# Detailed Instructions

## Toolchain

| Tool | Version | Why |
|---|---|---|
| Go | 1.25.0 | pinned in `go.work`; older versions reject the workspace |
| Python | 3.13 | pinned in `.python-version` |
| uv | recent | Python package manager; `uv.lock` is authoritative |
| Docker | 24+ | only for `examples/basic/` |
| protoc | 3.21+ | **optional** — only to regenerate `*.pb.go`, which are committed |

```bash
# Ubuntu 22.04 / 24.04
wget -O- https://go.dev/dl/go1.25.0.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH=$HOME/.local/bin:$PATH
uv python install 3.13

# macOS
brew install go@1.25 python@3.13 uv

# Fedora 41+
sudo dnf install -y golang python3.13 uv
```

Then build: `make build && uv sync --frozen`. Use `uv sync --frozen`, not
`uv pip install -e .` — the latter re-resolves dependencies and discards the
lockfile.

## Tiers

Every experiment runs through one script, `scripts/run_all_experiments.sh`, which
takes one or more named **parts** — `otel-demo`, `re2`, `uber`, and so on. We call
it *the driver* below. The tiers group those parts by what they cost you, so you
can pick a depth.

| Tier | What | Time | Needs |
|---|---|---|---|
| **T0** | The Getting Started steps above | ~15 min | Docker |
| **T1** | `otel-demo` — the main cost–fidelity result | ~4 min | 8 GB RAM, ~10 GB disk |
| **T2** | `re2` (RCA) and the otel-demo ablations | ~4 h | 8 GB RAM, ~40 GB disk |
| **T3** | `uber`, `uber-scalability`, `graph-ablation` | ~3 h | 16 GB RAM, **~300 GB disk** |

Times are measured at `NUM_SEEDS=1` (and `UBER_MAX_TRACES=200000` for T3) on a
CloudLab **c240g5**.

The driver has one further part, `strawman` (the TVAE baseline). It needs a CUDA
GPU, no Finding depends on it, and we are not asking you to run it.

## Fast path

Every part accepts these knobs. They default to the paper's exact configuration,
so leaving them unset reproduces what was published.

```bash
NUM_SEEDS=1 UBER_MAX_TRACES=200000 bash scripts/run_all_experiments.sh <part>
```

| Knob | Default | Fast path | Effect |
|---|---|---|---|
| `NUM_SEEDS` | 3 | 1 | Runs seed 42 only. Also drops head-sampling and Sifter to one iteration, so every approach keeps the same repetition count and the cost comparison stays apples-to-apples. |
| `ITERATIONS` | = `NUM_SEEDS` | — | How many times the randomized baselines (head sampling, Sifter) are repeated and averaged. It follows `NUM_SEEDS` automatically so every approach gets the same number of repetitions; setting it by hand makes the cost comparison uneven. Leave it alone. |
| `UBER_MAX_TRACES` | 500000 | 200000 | Caps the scalability sweep. |
| `DATA_DIR` / `OUTPUT_DIR` | `data` / `output` | — | Put datasets and results on a different filesystem, if the repo's does not have room. |

**Why we ran one seed, and suggest you do too.** The paper averages three seeds
(42, 43, 44) and reports that standard deviations stay **below 1%** on every
fidelity metric — the models are fit to the same distributions each time, so runs
differ only in sampling noise. Meanwhile `NUM_SEEDS=3` triples the work for every
approach, not just TPack. Paying 3× the runtime to move a fidelity score by
around a point is a poor trade when you are checking whether a claim holds. All
the results we ship are single-seed for this reason; `NUM_SEEDS=3` is there if
you want the error bars.

One caveat we found and would rather you heard from us: this holds for TPack's
own numbers, which reproduce within 1.2 points across all 15 cross-dataset cells,
but **not for the aggressive head-sampling baselines**. Head 1:50 graph fidelity
moved by up to 13 points between our single seed and the paper's 3-seed mean — a
1-in-50 sample rarely retains both endpoints of an edge, so which edges survive
swings hard between draws. If you want the baseline curve to match tightly rather
than just order correctly, run three seeds.

**Why capping Uber at 200K still reproduces Finding 2.** Uber is remapped into a
single 60-second bucket, so the trace count *is* the ingestion rate. 200,000
traces measured **186.2M spans in one 60-second epoch**, against the 187M
spans/min the paper reports for a single collector. Capping drops only the 500K
point — the most expensive in the sweep by a wide margin — while keeping the
claim itself and five of the six points on Figure 11.

## Running each tier

```bash
# The fast path, matching the tier times above and the results we ship.
# Unset it for the paper's 3-seed configuration; see Fast path above.
export NUM_SEEDS=1

# T1 — otel-demo (63 MB download, unpacks to 1.4 GB, SHA256-verified)
bash scripts/download_data.sh otel-demo
bash scripts/run_all_experiments.sh otel-demo

# T2 — RE2 plus the otel-demo ablations
bash scripts/download_data.sh re2                       # ~4 GB
bash scripts/run_all_experiments.sh re2
bash scripts/run_all_experiments.sh node-ablation feat-ablation

# T3 — Uber
bash scripts/download_data.sh uber1                     # 21 GB → 206 GB extracted
UBER_MAX_TRACES=200000 bash scripts/run_all_experiments.sh uber uber-scalability
bash scripts/run_all_experiments.sh graph-ablation

# Figures and tables, once the tiers you want are done
bash scripts/run_all_experiments.sh figures
uv run scorecard --input output/otel-demo/report.json --approaches tpack_default
```

The driver is idempotent — if it dies partway, re-running skips completed work.

**T3 storage.** Budget **~300 GB**: 206 GB of extracted Jaeger JSON, 26 GB of
per-size transforms, 41 GB of evaluation output. We ran it on a
CloudLab c240g5 with `DATA_DIR` and `OUTPUT_DIR` pointed at two large mounts;
that is what those knobs are for. If you do not have the space, run the small end
of the sweep instead (`UBER_MAX_TRACES=20000`, roughly 30 GB) — the monotonic
decline in unit cost is Finding 2, and the 200K row is only its most quotable
instance.

## The claims

Each row is a claim the paper makes, the command that produces the evidence, and
whether it held for us. Measured values are in
[`../results/reference/RESULTS.md`](../results/reference/RESULTS.md).

| # | What the paper claims | Run | Tier | Verdict |
|---|---|---|---|---|
| **1** | Breaks the cost–fidelity tradeoff: matches the fidelity of 1:3 head sampling at roughly the cost of 1:50, across datasets spanning 7–1,150 services | `otel-demo`, `re2`, `uber` | T1–T3 | **Reproduced** |
| **2** | Unit cost *decreases* as ingestion rate rises, and one edge collector sustains the paper's headline rate within a 60-second epoch | `uber-scalability` | T3 | **Reproduced** |
| **3** | Best cost–fidelity across most query types; duration is TPack's weakest | `otel-demo` | T1 | **Reproduced** |
| **4** | On RE2-OB, >60% RCA accuracy while beating every head-sampling rate, at 50× lower cost than head 1:1 | `re2` | T2 | **Partial** — accuracy and the beats-all-rates claim hold; the cost ratio does not (see below) |
| **5a** | **Node**-level modeling drives the gain: dropping key attributes, or adding a high-cardinality one, hurts the tradeoff | `node-ablation` | T2 | **Reproduced** |
| **5b** | **Graph**-level modeling drives the gain: template memorization buys fidelity at 1.5–5× the cost, and edge modeling scales better | `graph-ablation` | T3 | **Reproduced** |

`bash scripts/run_all_experiments.sh figures` renders all of it into
`output/paper-figures/`: `cost_fidelity.pdf` (Fig. 8), `query_fidelity.pdf`
(Fig. 9), `scalability.pdf` + `.tex` (Fig. 11 + table), `node_ablation.pdf`
(Fig. 12), `graph_ablation.pdf` (Fig. 13), `rca.pdf`, `tradeoff.pdf`, and
`cross_dataset.tex`.

## Where our numbers differ from the paper

Two things you should know before you compare, so neither surprises you.

**Model size on otel-demo.** The paper says ~520 KB of model parameters replace
67 MB of raw data, a ~130× reduction. Re-running on the published Zenodo dataset
gives **769 KB** across 35 one-minute buckets — 48% larger — or about **106×**
against 81 MB of gzipped OTLP. Both terms differ, and not by a common factor, so
this is not a units or scaling mismatch. We cannot reconcile it from the
published dataset and are flagging it rather than leaving you to find it. It is
the same order of magnitude and supports the same conclusion, and Finding 1
itself makes no claim about model size. We will correct the figure in the
camera-ready.

**Cost depends on your hardware, so every cost number will be higher than the
paper's.** Cost is `size_gb × $0.10/GB + cpu_seconds/3600 × $0.16/hr`, so it
bills the CPU time your machine actually spends. Head sampling barely uses any —
it forwards a subset — while TPack pays to fit the model. On our c240g5, **84% of
what TPack costs is CPU time** rather than bytes shipped; for head 1:1 it is 1%.
So a slower machine inflates TPack's cost almost proportionally while leaving the
baselines alone, and the ratio between them shrinks.

Everything ran substantially slower on the c240g5 than on the paper's
workstation — 2.4–3.0× on the scalability sweep. We think TPack is simply not
tuned for this machine: it was developed against a fast single-socket desktop
CPU, while the c240g5 is a dual-socket Xeon with slower cores, so there is likely
a CPU or memory bottleneck we have not chased down. Whatever the cause, more CPU
seconds means more cost, so TPack's cost advantage comes out smaller here than in
the paper.

That shows up most sharply in **Finding 4**, where the paper claims 50× lower
cost than head 1:1 on RE2-OB and we measure **12.8×**. Counting only bytes
shipped, which does not depend on hardware, TPack sends **76.8× less data** — so
the direction of the claim is intact. Full breakdown in RESULTS.md.

Practically: if you regenerate Figure 8 or 9, the fidelity (x) axis should line
up while the cost (y) axis sits higher, shifting every point the same way.
Compare the shape of the curve and TPack's position relative to head sampling,
not absolute dollar values.

Beyond that: TPack is generative, so expect small seed-driven deviations
everywhere, and larger ones on anything timing-related.

## Reference outputs

[`../results/reference/`](../results/reference/) holds the regenerated figures,
the figure-input JSON, and `RESULTS.md` with paper-value-beside-measured-value
tables for every claim. The full `report.json` files are too large for git; ask
us through HotCRP if you want them.

## Hardware

The paper's evaluation ran on a **single workstation: 8-core AMD Ryzen 7 9700X
(16 threads), 64 GB DDR5, NVIDIA RTX 3070 (8 GB, TVAE only)**. CloudLab appears
in the paper only as the three-node cluster used to *capture* the otel-demo
dataset, not to run the evaluation.

Our re-runs used a CloudLab **c240g5**: 2× Xeon Silver 4114 (40 threads), 192 GB
RAM, Ubuntu 24.04.

## Repo map

```
pkg/tpackmodel/              core models + algorithms (start table, topology,
                             GMM root timing, child timing, dependent attrs)
exporter/tpackexporter/      OTel Collector exporter — the compressor
receiver/tpackreceiver/      OTel Collector receiver — the decompressor
cmd/otelcol-tpack/           custom OTel Collector binary
cmd/tpack-eval/              evaluation CLI + all baselines (head, tail,
                             hindsight, sifter, OTel Arrow)
tpack_eval/                  Python: paper figures, scorecard, TVAE baseline
configs/                     per-dataset and per-ablation YAML
scripts/                     experiment driver, dataset download, smoke test
results/reference/           regenerated figures + measured-vs-paper tables
```

Architecture is documented in [`ARCHITECTURE.md`](ARCHITECTURE.md); every config
field in [`CONFIG.md`](CONFIG.md); dataset provenance in
[`DATASETS.md`](DATASETS.md).
