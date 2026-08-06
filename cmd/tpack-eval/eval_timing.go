package main

// evaluateTime writes timing results.
func evaluateTime(dir string, totalSeconds, decompressSeconds float64) error {
	result := map[string]any{
		"total_time_seconds":         totalSeconds,
		"compression_time_seconds":   totalSeconds - decompressSeconds,
		"decompression_time_seconds": decompressSeconds,
	}
	return writeEvalResult(dir, "time_results.json", result)
}
