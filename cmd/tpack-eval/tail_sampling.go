package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// classifyTraces partitions traces into "interesting" (error or high-latency) and "rest".
// A trace is interesting if any span has status.code == 2 (ERROR) or if its root
// duration exceeds the p95 threshold computed over all traces.
func classifyTraces(traces []*tpackmodel.Trace) (interesting, rest []*tpackmodel.Trace) {
	// Collect root durations for p95 calculation.
	type traceInfo struct {
		trace    *tpackmodel.Trace
		rootDur  int64
		hasError bool
	}
	infos := make([]traceInfo, 0, len(traces))

	for _, t := range traces {
		var rootDur int64
		var hasError bool
		for _, s := range t.Spans {
			if s.ParentSpanID == "" {
				rootDur = s.Duration
			}
			if s.Feature.IsError() {
				hasError = true
			}
		}
		infos = append(infos, traceInfo{trace: t, rootDur: rootDur, hasError: hasError})
	}

	// Compute p95 root duration.
	durations := make([]int64, len(infos))
	for i, info := range infos {
		durations[i] = info.rootDur
	}
	slices.Sort(durations)

	var p95Duration int64
	if len(durations) > 0 {
		idx := int(float64(len(durations)) * 0.95)
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		p95Duration = durations[idx]
	}

	// Partition.
	var errorCount, latencyCount int
	for _, info := range infos {
		isInteresting := info.hasError || info.rootDur > p95Duration
		if isInteresting {
			interesting = append(interesting, info.trace)
			if info.hasError {
				errorCount++
			}
			if info.rootDur > p95Duration {
				latencyCount++
			}
		} else {
			rest = append(rest, info.trace)
		}
	}

	log.Printf("classifyTraces: %d total, %d interesting (%.1f%%), %d error, %d high-latency (p95=%dus)",
		len(traces), len(interesting), float64(len(interesting))/float64(len(traces))*100,
		errorCount, latencyCount, p95Duration)

	return interesting, rest
}

// bucketTraces groups traces by time bucket and returns sorted keys + map.
func bucketTraces(traces []*tpackmodel.Trace, bucketDurationUs int64) ([]int64, map[int64][]*tpackmodel.Trace) {
	buckets := make(map[int64][]*tpackmodel.Trace)
	for _, t := range traces {
		bk := traceBucketKey(t, bucketDurationUs)
		buckets[bk] = append(buckets[bk], t)
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, buckets
}

// runTailSampling evaluates tail sampling: fidelity on biased subset, cost on ALL original data.
// Tail sampling sends all data to the backend, which then selects interesting traces.
func runTailSampling(inputPath, baseOutputDir string, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) error {
	buckets, err := readOTLP(inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	// Flatten all traces.
	var allTraces []*tpackmodel.Trace
	for _, traces := range buckets {
		allTraces = append(allTraces, traces...)
	}
	log.Printf("Tail sampling: %d traces total", len(allTraces))

	interesting, _ := classifyTraces(allTraces)

	outputDir := filepath.Join(baseOutputDir, "tail")
	log.Printf("Tail sampling → %s (%d interesting traces, cost based on %d total)", outputDir, len(interesting), len(allTraces))

	// Write biased subset as dataset (for fidelity evaluation).
	subsetKeys, subsetBuckets := bucketTraces(interesting, bucketDurationUs)
	datasetDir := filepath.Join(outputDir, "dataset")
	if err := writeSampledOTLP(datasetDir, subsetKeys, subsetBuckets); err != nil {
		return fmt.Errorf("write OTLP for tail: %w", err)
	}

	// Write ALL original traces as OTLP JSON to a temp dir for gzip benchmarking.
	// Tail sampling transmits everything to the backend.
	allKeys, allBuckets := bucketTraces(allTraces, bucketDurationUs)
	fullDataDir := filepath.Join(outputDir, "full_data_tmp")
	if err := writeSampledOTLP(fullDataDir, allKeys, allBuckets); err != nil {
		return fmt.Errorf("write full OTLP for tail: %w", err)
	}

	// Benchmark gzip on ALL data for timing AND write gzipped output for
	// size-based cost accounting (tail transmits everything to the backend).
	compressedDir := filepath.Join(outputDir, "compressed", "data")
	gzResult, err := benchmarkGzip(fullDataDir, 0, true, compressedDir)
	if err != nil {
		log.Printf("Warning: gzip bench failed for tail: %v", err)
	}

	// Write timing files.
	if gzResult.CompressSeconds > 0 || gzResult.DecompressSeconds > 0 {
		if err := writeTimingFiles(compressedDir, gzResult.CompressSeconds, 0, gzResult.DecompressSeconds, 0, 0, 0); err != nil {
			log.Printf("Warning: write timing for tail: %v", err)
		}
	}

	// Clean up temp dir.
	os.RemoveAll(fullDataDir)

	return nil
}

// runHindsightSampling evaluates Hindsight: fidelity on biased subset, cost on biased subset only.
// Hindsight buffers traces locally and only transmits triggered/selected traces.
func runHindsightSampling(inputPath, baseOutputDir string, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) error {
	buckets, err := readOTLP(inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	// Flatten all traces.
	var allTraces []*tpackmodel.Trace
	for _, traces := range buckets {
		allTraces = append(allTraces, traces...)
	}
	log.Printf("Hindsight sampling: %d traces total", len(allTraces))

	interesting, _ := classifyTraces(allTraces)

	outputDir := filepath.Join(baseOutputDir, "hindsight")
	log.Printf("Hindsight sampling → %s (%d interesting traces)", outputDir, len(interesting))

	// Write biased subset as dataset (for fidelity evaluation).
	subsetKeys, subsetBuckets := bucketTraces(interesting, bucketDurationUs)
	datasetDir := filepath.Join(outputDir, "dataset")
	if err := writeSampledOTLP(datasetDir, subsetKeys, subsetBuckets); err != nil {
		return fmt.Errorf("write OTLP for hindsight: %w", err)
	}

	// Benchmark gzip on subset for timing AND write gzipped output for
	// cost accounting. Hindsight only transmits the selected (subset) traces.
	compressedDir := filepath.Join(outputDir, "compressed", "data")
	gzResult, err := benchmarkGzip(datasetDir, 0, true, compressedDir)
	if err != nil {
		log.Printf("Warning: gzip bench failed for hindsight: %v", err)
	} else {
		if err := writeTimingFiles(compressedDir, gzResult.CompressSeconds, 0, gzResult.DecompressSeconds, 0, 0, 0); err != nil {
			log.Printf("Warning: write timing for hindsight: %v", err)
		}
	}

	return nil
}
