package main

// evaluateError counts error spans (status.code == "2") per group per minute bucket.
// Same grouping as rate_over_time but filtered to error spans only.
// Output: {"span_rate_by_time": {...}, "total_spans_by_group": {...}}
func evaluateError(dir string, traces []evalTrace) error {
	spanCountByTime := countSpansParallel(traces, func(span *evalSpan) bool {
		// Filter: only error spans (status.code == "2")
		// Check both Feature (when status.code is a feature column) and
		// Metadata (when status.code is a metadata column)
		statusCode := span.Feature.StatusCode()
		if statusCode == "" {
			statusCode = metadataGet(span.Metadata, "status.code")
		}
		return statusCode == "2"
	})

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

	return writeEvalResult(dir, "error_over_time_results.json", result)
}
