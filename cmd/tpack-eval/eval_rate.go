package main

import (
	"runtime"
	"sync"
)

// evaluateRate counts all spans per group per minute bucket.
// Groups: "all", "service.name:X", and for each metadata column "<col>:<val>" and "service.name:X!@#<col>:<val>"
// Output: {"span_rate_by_time": {...}, "total_spans_by_group": {...}}
func evaluateRate(dir string, traces []evalTrace) error {
	spanCountByTime := countSpansParallel(traces, func(span *evalSpan) bool { return true })

	totalByGroup := make(map[string]int, len(spanCountByTime))
	for group, buckets := range spanCountByTime {
		total := 0
		for _, count := range buckets {
			total += count
		}
		totalByGroup[group] = total
	}

	result := map[string]any{
		"span_rate_by_time":    spanCountByTime,
		"total_spans_by_group": totalByGroup,
	}

	return writeEvalResult(dir, "rate_over_time_results.json", result)
}

// countSpansParallel shards traces across workers, builds per-worker maps,
// then merges. filter decides whether to include a span.
func countSpansParallel(traces []evalTrace, filter func(*evalSpan) bool) map[string]map[int64]int {
	nw := max(min(runtime.NumCPU(), len(traces)), 1)

	partials := make([]map[string]map[int64]int, nw)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make(map[string]map[int64]int)
			for i := w; i < len(traces); i += nw {
				for _, span := range traces[i] {
					if !filter(span) {
						continue
					}
					bucket := timeBucketMinute(span.StartTime)
					for _, group := range metadataGroupKeys(span.Feature, span.Metadata) {
						m, ok := local[group]
						if !ok {
							m = make(map[int64]int)
							local[group] = m
						}
						m[bucket]++
					}
				}
			}
			partials[w] = local
		}(w)
	}
	wg.Wait()

	// Merge partials
	total := make(map[string]map[int64]int)
	for _, local := range partials {
		for group, buckets := range local {
			m, ok := total[group]
			if !ok {
				m = make(map[int64]int)
				total[group] = m
			}
			for bucket, count := range buckets {
				m[bucket] += count
			}
		}
	}
	return total
}
