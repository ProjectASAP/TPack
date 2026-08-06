package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// evalSpan is a simplified span for evaluation (matches Python dataset's span dict).
type evalSpan struct {
	SpanID       string
	ParentSpanID string // "" for root spans
	Feature      tpackmodel.SpanFeature
	Metadata     map[string]string // dynamic metadata (e.g. "http.status_code" → "200")
	StartTime    int64             // microseconds
	Duration     int64             // microseconds
}

// evalTrace maps spanID → evalSpan for a single trace.
type evalTrace map[string]*evalSpan

// originalToEvalTraces converts original input traces to evalTraces.
func originalToEvalTraces(buckets map[int64][]*tpackmodel.Trace) []evalTrace {
	// Flatten buckets → []*tpackmodel.Trace for shard-able iteration.
	var flat []*tpackmodel.Trace
	for _, bucket := range buckets {
		flat = append(flat, bucket...)
	}

	nw := max(min(runtime.NumCPU(), len(flat)), 1)

	traces := make([]evalTrace, len(flat))
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(flat); i += nw {
				td := flat[i]
				et := make(evalTrace, len(td.Spans))
				for spanID, sd := range td.Spans {
					et[spanID] = &evalSpan{
						SpanID:       sd.SpanID,
						ParentSpanID: sd.ParentSpanID,
						Feature:      sd.Feature,
						// Lazy: metadataGroupKeys iterates both Feature + Metadata
						// without merging, saving one map allocation per span.
						Metadata:  sd.Metadata,
						StartTime: sd.StartTime,
						Duration:  sd.Duration,
					}
				}
				traces[i] = et
			}
		}(w)
	}
	wg.Wait()
	return traces
}

// metadataGet safely gets a value from a metadata map.
func metadataGet(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

// metadataGroupKeys returns all group keys for a span's feature + metadata.
// Iterates both sources lazily without allocating a merged map. Metadata
// values take precedence over feature values when keys overlap.
// Level 0: "all"
// Level 1: "col:val" for each non-empty column
// Level 2: all pairwise combinations "col1:val1!@#col2:val2" (sorted by column)
func metadataGroupKeys(feat tpackmodel.SpanFeature, metadata map[string]string) []string {
	keys := []string{"all"}

	// Collect non-empty "col:val" entries from both sources (metadata wins on conflict).
	// Track which keys metadata already covered so we skip the same key from Feature.
	var entries []string
	for col, val := range metadata {
		if val == "" {
			continue
		}
		entries = append(entries, col+":"+val)
	}
	feat.Range(func(col, val string) bool {
		if val == "" {
			return true
		}
		if _, exists := metadata[col]; exists {
			return true // metadata already covered this key
		}
		entries = append(entries, col+":"+val)
		return true
	})
	sort.Strings(entries)

	// Level 1
	keys = append(keys, entries...)

	// Level 2: all pairs (i < j)
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			keys = append(keys, entries[i]+"!@#"+entries[j])
		}
	}

	return keys
}

// timeBucketMinute converts a microsecond timestamp to a minute bucket.
func timeBucketMinute(startTimeUs int64) int64 {
	return startTimeUs / 60_000_000
}

// writeEvalResult marshals result to JSON and writes it to dir/filename.
func writeEvalResult(dir, filename string, result any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filename, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// evalResultExists checks if an evaluator's output file already exists.
func evalResultExists(dir, filename string) bool {
	_, err := os.Stat(filepath.Join(dir, filename))
	return err == nil
}

// runAllEvaluators runs evaluators and writes JSON results.
// Evaluators whose output files already exist are skipped, except RCA evaluators
// are always re-run when injectTimeUs is non-nil (since inject_time changes their output).
func runAllEvaluators(evaluatedDir string, traces []evalTrace, injectTimeUs *int64, cpuSeconds, decompressSeconds float64) error {
	log.Printf("Running evaluators, writing to %s ...", evaluatedDir)

	ran := 0

	type evaluator struct {
		name string
		file string
		run  func() error
	}

	evals := []evaluator{
		{"graph", "graph_results.json", func() error { return evaluateGraph(evaluatedDir, traces) }},
		{"rate", "rate_over_time_results.json", func() error { return evaluateRate(evaluatedDir, traces) }},
		{"error", "error_over_time_results.json", func() error { return evaluateError(evaluatedDir, traces) }},
		{"duration", "duration_over_time_results.json", func() error { return evaluateDuration(evaluatedDir, traces) }},
		{"span_count", "span_count_results.json", func() error { return evaluateSpanCount(evaluatedDir, traces) }},
		{"time", "time_results.json", func() error { return evaluateTime(evaluatedDir, cpuSeconds, decompressSeconds) }},
		{"trace_rca", "trace_rca_results.json", func() error { return evaluateTraceRCA(evaluatedDir, traces, injectTimeUs) }},
		{"micro_rank", "micro_rank_results.json", func() error { return evaluateMicroRank(evaluatedDir, traces, injectTimeUs) }},
		{"anomaly_detection", "anomaly_detection_results.json", func() error { return evaluateAnomalyDetection(evaluatedDir, traces, injectTimeUs) }},
	}

	// Evaluators are independent (read-only input, write to separate files),
	// so they can run concurrently.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	timings := make(map[string]float64)
	for _, e := range evals {
		if evalResultExists(evaluatedDir, e.file) {
			continue
		}
		ran++
		wg.Add(1)
		go func(e evaluator) {
			defer wg.Done()
			t := time.Now()
			err := e.run()
			elapsed := time.Since(t).Seconds()
			mu.Lock()
			timings[e.name] = elapsed
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", e.name, err)
			}
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// Log per-evaluator timings sorted by duration (largest first)
	type timingEntry struct {
		name string
		dur  float64
	}
	entries := make([]timingEntry, 0, len(timings))
	for name, dur := range timings {
		entries = append(entries, timingEntry{name, dur})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].dur > entries[j].dur })
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s=%.1fs", e.name, e.dur))
	}
	log.Printf("Evaluators complete: %d/%d ran (%d skipped) — %s",
		ran, len(evals), len(evals)-ran, strings.Join(parts, ", "))
	return nil
}
