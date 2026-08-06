package main

import (
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// runSifterSampling executes Sifter biased sampling for all rate x iteration combinations.
// Sifter learns common trace patterns via a paragraph vector model and biases
// sampling toward unusual (high prediction error) traces.
func runSifterSampling(inputPath, baseOutputDir string, rates []int, iterations int, seed int64, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) error {
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

	// Flatten all traces sorted by start time (temporal order for online processing)
	allTraces := make([]*tpackmodel.Trace, 0, totalTraces)
	for _, bk := range bucketKeys {
		allTraces = append(allTraces, buckets[bk]...)
	}
	sort.Slice(allTraces, func(i, j int) bool {
		return traceMinStart(allTraces[i]) < traceMinStart(allTraces[j])
	})

	// Sifter, deployed at the edge collector, transmits only the sampled subset.
	// Cost = gzip(sampled subset) + model training CPU.

	// Pre-fit encoder on all traces' features for stable vocabulary
	encoder := tpackmodel.NewNodeEncoder()
	var allFeatures []tpackmodel.SpanFeature
	for _, t := range allTraces {
		for _, s := range t.Spans {
			allFeatures = append(allFeatures, s.Feature)
		}
	}
	encoder.Fit(allFeatures)
	vocabSize := int(encoder.VocabSize())
	log.Printf("Sifter: vocabulary size = %d span types", vocabSize)

	// Serial across (rate, iter): each iteration is already intra-parallel (see
	// model.forward / model.processPath), but the sliding-window sampling
	// decision is inherently sequential at the iteration level.
	for _, rate := range rates {
		for iter := 1; iter <= iterations; iter++ {
			if err := runSifterOne(rate, iter, baseOutputDir, seed, bucketDurationUs,
				vocabSize, allTraces, encoder); err != nil {
				return err
			}
		}
	}
	return nil
}

// runSifterOne executes a single (rate, iter) sifter run. Each call creates its
// own model/rng/sampled slice, so concurrent invocations are safe.
func runSifterOne(rate, iter int, baseOutputDir string, seed, bucketDurationUs int64,
	vocabSize int, allTraces []*tpackmodel.Trace, encoder *tpackmodel.NodeEncoder) error {
	dirName := fmt.Sprintf("sifter_%d_%d", rate, iter)
	outputDir := filepath.Join(baseOutputDir, dirName)
	log.Printf("Sifter sampling rate=1/%d iteration=%d -> %s", rate, iter, dirName)

	alpha := 1.0 / float64(rate)
	uniqueSeed := seed + int64(rate)*1000 + int64(iter)
	rng := rand.New(rand.NewSource(uniqueSeed))

	model := newSifterModel(vocabSize, alpha, rng)

	start := time.Now()
	var sampled []*tpackmodel.Trace
	var totalPaths, shallowTraces int

	for ti, t := range allTraces {
		if ti > 0 && ti%5000 == 0 {
			log.Printf("  [iter %d] Sifter: processed %d/%d traces (%.1f%%), %d sampled so far, %.1fs elapsed",
				iter, ti, len(allTraces), float64(ti)/float64(len(allTraces))*100,
				len(sampled), time.Since(start).Seconds())
		}
		paths := extractPaths(t, encoder, 5)
		totalPaths += len(paths)

		var traceLoss float64
		if len(paths) == 0 {
			shallowTraces++
			traceLoss = 0.0
		} else {
			sumLoss := 0.0
			for _, path := range paths {
				ctx, target := pathContextAndTarget(path)
				sumLoss += model.processPath(ctx, target)
			}
			traceLoss = sumLoss / float64(len(paths))
		}

		prob := model.samplingProbability(traceLoss)
		if rng.Float64() < prob {
			sampled = append(sampled, t)
		}
	}
	compCPU := time.Since(start).Seconds()

	log.Printf("  [iter %d] Sifter: %d/%d traces sampled (%.1f%%), %d total paths, %d shallow, %.2fs CPU",
		iter, len(sampled), len(allTraces),
		float64(len(sampled))/float64(len(allTraces))*100,
		totalPaths, shallowTraces, compCPU)

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

	timingDir := filepath.Join(outputDir, "compressed", "data")
	gzResult, err := benchmarkGzip(datasetDir, 0, true, timingDir)
	if err != nil {
		log.Printf("Warning: gzip bench failed for %s: %v", dirName, err)
	}
	if gzResult.CompressSeconds > 0 || gzResult.DecompressSeconds > 0 {
		if err := writeTimingFiles(timingDir, gzResult.CompressSeconds+compCPU, 0, gzResult.DecompressSeconds, 0, 0, 0); err != nil {
			log.Printf("Warning: write timing for %s: %v", dirName, err)
		}
	}

	return nil
}

// traceMinStart returns the earliest span start time in a trace.
func traceMinStart(t *tpackmodel.Trace) int64 {
	minStart := int64(1<<63 - 1)
	for _, s := range t.Spans {
		if s.StartTime < minStart {
			minStart = s.StartTime
		}
	}
	return minStart
}
