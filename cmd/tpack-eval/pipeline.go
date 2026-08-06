package main

import (
	"log"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// bucketResult holds the output of processing a single time bucket.
type bucketResult struct {
	BucketKey          int64
	Spans              []tpackmodel.GeneratedSpan
	SpanCount          int                    // total generated spans (valid even when Spans is nil)
	ModelBytes         []byte                 // serialized model for cost calculation
	GenerateSeconds    float64                // wall-clock time for GenerateBucket (decompression)
	Encoder            *tpackmodel.NodeEncoder // for inverse transform during output writing
	InputTraces        int                    // number of input traces used for training
	InputSpans         int                    // number of input spans used for training
	IOWallSeconds      float64                // wall time of I/O phase (reading chunks)
	ComputeWallSeconds float64                // wall time of compute phase (train + generate, excludes I/O)
}

// processBucket trains models on the given traces and generates synthetic traces.
func processBucket(bucketKey int64, traces []*tpackmodel.Trace, config tpackmodel.TPackConfig, primaryAttributes, dependentAttributes []string) bucketResult {
	inputSpans := 0
	inputTraces := len(traces)
	for _, t := range traces {
		inputSpans += len(t.Spans)
	}

	state, err := tpackmodel.TrainBucket(traces, config, primaryAttributes, dependentAttributes)
	if err != nil {
		log.Fatalf("training failed for bucket %d: %v", bucketKey, err)
	}

	tGen := time.Now()
	spans, spanCount := tpackmodel.GenerateBucket(state, tpackmodel.GenerateOptions{BucketKey: bucketKey})
	genSec := time.Since(tGen).Seconds()

	modelBytes, _ := state.Marshal()
	return bucketResult{
		BucketKey:       bucketKey,
		Spans:           spans,
		SpanCount:       spanCount,
		ModelBytes:      modelBytes,
		GenerateSeconds: genSec,
		Encoder:         state.NodeEncoder,
		InputTraces:     inputTraces,
		InputSpans:      inputSpans,
	}
}
