package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// runHeadSampling executes head sampling for all rate × iteration combinations.
//
// Rate 1 is deterministic (keep every trace), so iterations are identical;
// we materialize iteration 1 via hardlinks (no in-memory copy) and symlink
// iterations 2..N to it. Remaining rates load the dataset once and sweep
// iterations × rates the usual way.
func runHeadSampling(inputPath, baseOutputDir string, rates []int, iterations int, seed int64, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) error {
	var hasRate1 bool
	var otherRates []int
	for _, r := range rates {
		if r == 1 {
			hasRate1 = true
		} else {
			otherRates = append(otherRates, r)
		}
	}

	// Phase 1: rate 1 via hardlinks — no dataset load, no sampling pass.
	if hasRate1 {
		if err := materializeRate1(inputPath, baseOutputDir, iterations); err != nil {
			return fmt.Errorf("rate-1 materialize: %w", err)
		}
	}

	if len(otherRates) == 0 {
		return nil
	}

	// Phase 2: load the input once, run the remaining rates × iterations.
	buckets, err := readOTLP(inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	totalTraces := 0
	for _, traces := range buckets {
		totalTraces += len(traces)
	}
	log.Printf("Read %d traces in %d buckets", totalTraces, len(buckets))

	bucketKeys := make([]int64, 0, len(buckets))
	for k := range buckets {
		bucketKeys = append(bucketKeys, k)
	}
	slices.Sort(bucketKeys)

	// Flatten all traces for sampling (we'll re-bucket after sampling)
	allTraces := make([]*tpackmodel.Trace, 0, totalTraces)
	for _, bk := range bucketKeys {
		allTraces = append(allTraces, buckets[bk]...)
	}

	for _, rate := range otherRates {
		for iter := 1; iter <= iterations; iter++ {
			dirName := fmt.Sprintf("head_%d_%d", rate, iter)
			outputDir := filepath.Join(baseOutputDir, dirName)
			log.Printf("Sampling rate=1/%d iteration=%d → %s", rate, iter, dirName)

			uniqueSeed := seed + int64(rate)*1000 + int64(iter)
			rng := rand.New(rand.NewSource(uniqueSeed))

			sampled := sampleTraces(allTraces, rate, rng)
			log.Printf("  Sampled %d/%d traces", len(sampled), len(allTraces))

			// Re-bucket sampled traces
			sampledBuckets := make(map[int64][]*tpackmodel.Trace)
			for _, t := range sampled {
				bk := traceBucketKey(t, bucketDurationUs)
				sampledBuckets[bk] = append(sampledBuckets[bk], t)
			}

			sampledBucketKeys := make([]int64, 0, len(sampledBuckets))
			for k := range sampledBuckets {
				sampledBucketKeys = append(sampledBucketKeys, k)
			}
			slices.Sort(sampledBucketKeys)

			datasetDir := filepath.Join(outputDir, "dataset")
			if err := writeSampledOTLP(datasetDir, sampledBucketKeys, sampledBuckets); err != nil {
				return fmt.Errorf("write OTLP for %s: %w", dirName, err)
			}

			// Benchmark gzip on the chunks and also persist gzipped output
			// as model_bucket_* for size-based cost accounting.
			compressedDir := filepath.Join(outputDir, "compressed", "data")
			gzResult, err := benchmarkGzip(datasetDir, 0, true, compressedDir)
			if err != nil {
				log.Printf("Warning: gzip bench failed for %s: %v", dirName, err)
			} else {
				if err := writeTimingFiles(compressedDir, gzResult.CompressSeconds, 0, gzResult.DecompressSeconds, 0, 0, 0); err != nil {
					log.Printf("Warning: write timing for %s: %v", dirName, err)
				}
			}

		}
	}

	// Release the in-memory dataset explicitly; helps if this function is
	// reused in a longer-lived process.
	allTraces = nil
	buckets = nil
	_ = allTraces
	_ = buckets

	return nil
}

// materializeRate1 populates head_1_1/dataset/ with hardlinks to the source
// chunk files (rate 1 = keep every trace, deterministic), runs the gzip
// benchmark, then symlinks head_1_2..head_1_N to head_1_1. Zero in-memory
// dataset duplication, zero extra disk space for iterations 2..N.
func materializeRate1(inputPath, baseOutputDir string, iterations int) error {
	dir1 := filepath.Join(baseOutputDir, "head_1_1")
	datasetDir := filepath.Join(dir1, "dataset")
	compressedDir := filepath.Join(dir1, "compressed", "data")

	var inputChunks []string
	for _, pat := range []string{"*.pb", "*.json"} {
		matches, err := filepath.Glob(filepath.Join(inputPath, pat))
		if err != nil {
			return fmt.Errorf("glob input chunks (%s): %w", pat, err)
		}
		inputChunks = append(inputChunks, matches...)
	}
	if len(inputChunks) == 0 {
		return fmt.Errorf("no chunk files in %s", inputPath)
	}
	sort.Strings(inputChunks)

	if err := os.MkdirAll(datasetDir, 0755); err != nil {
		return fmt.Errorf("mkdir dataset: %w", err)
	}

	linkedCount := 0
	for _, src := range inputChunks {
		dst := filepath.Join(datasetDir, filepath.Base(src))
		if _, err := os.Stat(dst); err == nil {
			// Already present — idempotent rerun.
			linkedCount++
			continue
		}
		if err := os.Link(src, dst); err != nil {
			// Fall back to symlink if hardlink across filesystems fails
			// (EXDEV). Use an absolute source path so the symlink resolves
			// regardless of the destination's location.
			absSrc, aerr := filepath.Abs(src)
			if aerr != nil {
				absSrc = src
			}
			if serr := os.Symlink(absSrc, dst); serr != nil {
				return fmt.Errorf("link %s -> %s: %w (symlink also failed: %v)", src, dst, err, serr)
			}
		}
		linkedCount++
	}
	log.Printf("materializeRate1: linked %d chunk files into %s", linkedCount, datasetDir)

	// Gzip benchmark — same path as the regular rate loop.
	gzResult, err := benchmarkGzip(datasetDir, 0, true, compressedDir)
	if err != nil {
		log.Printf("Warning: gzip bench failed for rate-1: %v", err)
	} else {
		if err := writeTimingFiles(compressedDir, gzResult.CompressSeconds, 0, gzResult.DecompressSeconds, 0, 0, 0); err != nil {
			log.Printf("Warning: write timing for rate-1: %v", err)
		}
	}

	// Symlink iterations 2..N to head_1_1 using a relative path so the
	// output tree remains relocatable.
	for iter := 2; iter <= iterations; iter++ {
		iterDir := filepath.Join(baseOutputDir, fmt.Sprintf("head_1_%d", iter))
		if fi, err := os.Lstat(iterDir); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				continue // already a symlink — idempotent
			}
			// Stale real directory from a prior run — replace with symlink.
			if err := os.RemoveAll(iterDir); err != nil {
				return fmt.Errorf("remove stale %s: %w", iterDir, err)
			}
		}
		if err := os.Symlink("head_1_1", iterDir); err != nil {
			return fmt.Errorf("symlink %s -> head_1_1: %w", iterDir, err)
		}
	}

	return nil
}

// traceBucketKey computes the time bucket key for a trace based on its earliest span.
func traceBucketKey(t *tpackmodel.Trace, bucketDurationUs int64) int64 {
	minStart := int64(1<<63 - 1)
	for _, s := range t.Spans {
		if s.StartTime < minStart {
			minStart = s.StartTime
		}
	}
	return minStart / bucketDurationUs
}

// sampleTraces keeps each trace with probability 1/rate.
func sampleTraces(traces []*tpackmodel.Trace, rate int, rng *rand.Rand) []*tpackmodel.Trace {
	threshold := 1.0 / float64(rate)
	var sampled []*tpackmodel.Trace
	for _, t := range traces {
		if rng.Float64() < threshold {
			sampled = append(sampled, t)
		}
	}
	return sampled
}

// writeSampledOTLP writes sampled spans as OTLP JSON.
func writeSampledOTLP(dir string, bucketKeys []int64, sampledBuckets map[int64][]*tpackmodel.Trace) error {
	// Flatten buckets → []*tpackmodel.Trace, then delegate chunking/marshaling/write to
	// the shared helper. Avoids single-file OOM on large samples (e.g., rate=1).
	var allTraces []*tpackmodel.Trace
	for _, bk := range bucketKeys {
		allTraces = append(allTraces, sampledBuckets[bk]...)
	}
	return writeOTLPChunksParallel(
		dir,
		allTraces,
		func(t *tpackmodel.Trace) string { return t.TraceID },
		func(chunk []*tpackmodel.Trace) ptrace.Traces { return convertSampledToPdata(chunk) },
	)
}

// convertSampledToPdata converts sampled tpackmodel.Trace to ptrace.Traces for OTLP serialization.
func convertSampledToPdata(traces []*tpackmodel.Trace) ptrace.Traces {
	var spans []otlpconv.SpanData
	for _, t := range traces {
		for _, s := range t.Spans {
			spans = append(spans, otlpconv.SpanData{
				TraceID:      t.TraceID,
				SpanID:       s.SpanID,
				ParentSpanID: s.ParentSpanID,
				Feature:      s.Feature,
				StartTime:    s.StartTime,
				Duration:     s.Duration,
				Metadata:     s.Metadata,
			})
		}
	}

	td := ptrace.NewTraces()
	otlpconv.AppendSpans(td, spans)
	return td
}

// parseRates parses a comma-separated string of sampling rates.
func parseRates(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	rates := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		r, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid rate %q: %w", p, err)
		}
		if r < 1 {
			return nil, fmt.Errorf("rate must be >= 1, got %d", r)
		}
		rates = append(rates, r)
	}
	if len(rates) == 0 {
		return nil, fmt.Errorf("no sampling rates provided")
	}
	return rates, nil
}
