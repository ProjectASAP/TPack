package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// writeOutputOTLP writes output spans as OTLP proto binary,
// using per-bucket encoders for correct inverse transform.
func writeOutputOTLP(
	outputDir string,
	bucketKeys []int64,
	results []bucketResult,
) error {
	// Pre-resolve each span's feature once (cheap string lookup; shared across chunking).
	var items []otlpconv.SpanData
	for i := range bucketKeys {
		encoder := results[i].Encoder
		for _, s := range results[i].Spans {
			items = append(items, otlpconv.SpanData{
				TraceID:      s.TraceID,
				SpanID:       s.SpanID,
				ParentSpanID: s.ParentSpanID,
				Feature:      encoder.InverseTransform(s.NodeIdx),
				StartTime:    s.StartTime,
				Duration:     s.Duration,
				Metadata:     s.Metadata,
			})
		}
	}

	return writeOTLPChunksParallel(
		outputDir,
		items,
		func(it otlpconv.SpanData) string { return it.TraceID },
		func(chunk []otlpconv.SpanData) ptrace.Traces {
			td := ptrace.NewTraces()
			otlpconv.AppendSpans(td, chunk)
			return td
		},
	)
}

// writeOTLPChunksParallel partitions items by TraceID hash into NumCPU chunks,
// builds + marshals + writes each chunk as chunk_%04d.pb in parallel.
// Spans of the same trace stay in the same chunk so downstream trace
// reconstruction works per-chunk. Empty chunks are skipped.
func writeOTLPChunksParallel[T any](
	dir string,
	items []T,
	traceID func(T) string,
	populateChunk func([]T) ptrace.Traces,
) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	numChunks := max(runtime.NumCPU(), 1)
	chunks := make([][]T, numChunks)
	for _, it := range items {
		tid := traceID(it)
		// FNV-1a hash of the full TraceID for balanced chunking, even when
		// TraceIDs share a common prefix (e.g., Uber data all starts with 0x00).
		h := fnv.New32a()
		h.Write([]byte(tid))
		ch := int(h.Sum32()) % numChunks
		if ch < 0 {
			ch += numChunks
		}
		chunks[ch] = append(chunks[ch], it)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numChunks)
	for ci := range numChunks {
		if len(chunks[ci]) == 0 {
			continue
		}
		wg.Add(1)
		go func(ci int) {
			defer wg.Done()
			td := populateChunk(chunks[ci])
			data, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
			if err != nil {
				errCh <- fmt.Errorf("marshal chunk %d: %w", ci, err)
				return
			}
			path := filepath.Join(dir, fmt.Sprintf("chunk_%04d.pb", ci))
			if err := os.WriteFile(path, data, 0o644); err != nil {
				errCh <- fmt.Errorf("write %s: %w", path, err)
			}
		}(ci)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}
