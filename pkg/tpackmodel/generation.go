package tpackmodel

import (
	"log"
	mathrand "math/rand"
	"runtime"
	"sync"
)

// GeneratedSpan is a span produced by GenerateBucket. The TraceID is embedded
// so the result can be a flat slice that callers group/chunk as needed.
type GeneratedSpan struct {
	TraceID      string
	SpanID       string
	ParentSpanID string // empty for root spans
	NodeIdx      int32
	StartTime    int64 // microseconds
	Duration     int64 // microseconds
	Metadata     map[string]string
}

// GenerateOptions controls a single GenerateBucket call.
type GenerateOptions struct {
	// BucketKey is mixed into the seed so each bucket gets unique trace IDs
	// even when reusing the same model with the same RandomSeed.
	BucketKey int64

	// Workers is the number of parallel goroutines to use. Zero means
	// runtime.NumCPU(). Each worker gets its own RNG seeded with
	// (RandomSeed + BucketKey*31 + worker_index).
	Workers int

	// Rng overrides the master RNG used for root-feature sampling and
	// shuffle (template mode). Worker RNGs are still derived from the seed
	// formula. If nil, a master RNG is built from RandomSeed + BucketKey*31.
	Rng *mathrand.Rand

	// DiscardSpans=true generates traces for timing measurement but does not
	// accumulate the resulting spans across workers. The returned slice is
	// nil; only the count is meaningful.
	DiscardSpans bool
}

// GenerateBucket runs the 4-stage generation pipeline and returns the
// generated spans plus the total span count.
//
// Stages:
//  1. Sample root features (exact counts from training, or template playback).
//  2. For each root, generate the topology tree and root duration.
//  3. (folded into 4) Sample root metadata with NO_PARENT sentinel.
//  4. Level-by-level batched metadata sampling for all child spans.
//
// The state is read-only during generation; multiple goroutines safely share it.
func GenerateBucket(state *TPackModelState, opts GenerateOptions) ([]GeneratedSpan, int) {
	seed := int64(state.Config.RandomSeed) + opts.BucketKey*31

	masterRng := opts.Rng
	if masterRng == nil {
		masterRng = mathrand.New(mathrand.NewSource(seed))
	}

	var rootSamples []RootSample
	if state.Config.TopologyMode == "template" {
		rootSamples = state.TopologyModel.GetAllTemplateSamples()
		// Shuffle for randomized timing assignment.
		masterRng.Shuffle(len(rootSamples), func(i, j int) {
			rootSamples[i], rootSamples[j] = rootSamples[j], rootSamples[i]
		})
	} else {
		rootSamples = state.StartTableModel.SampleRootFeaturesStratified(masterRng)
	}

	log.Printf("    generate: %d rootSamples (topologyMode=%s)", len(rootSamples), state.Config.TopologyMode)

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(rootSamples) {
		workers = len(rootSamples)
	}
	if workers < 1 {
		workers = 1
	}

	shards := make([][]RootSample, workers)
	for i, s := range rootSamples {
		shards[i%workers] = append(shards[i%workers], s)
	}

	if opts.DiscardSpans {
		counts := make([]int, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				workerRng := mathrand.New(mathrand.NewSource(seed + int64(w)))
				counts[w] = generateShardCount(state, shards[w], workerRng)
			}(w)
		}
		wg.Wait()
		total := 0
		for _, c := range counts {
			total += c
		}
		return nil, total
	}

	results := make([][]GeneratedSpan, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			workerRng := mathrand.New(mathrand.NewSource(seed + int64(w)))
			results[w] = generateShard(state, shards[w], workerRng)
		}(w)
	}
	wg.Wait()

	var allSpans []GeneratedSpan
	for _, r := range results {
		allSpans = append(allSpans, r...)
	}
	return allSpans, len(allSpans)
}
