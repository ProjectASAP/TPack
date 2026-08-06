package main

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
)

// evaluateDuration computes duration percentiles per group per minute bucket.
// Percentiles: p0, p10, p20, ..., p100 using numpy-compatible linear interpolation.
// Output: {"duration_percentiles_by_time": {...}, "total_spans_by_group": {...}}
func evaluateDuration(dir string, traces []evalTrace) error {
	// Phase 1: parallel collection — shard traces across workers.
	nw := max(min(runtime.NumCPU(), len(traces)), 1)

	partials := make([]map[string]map[int64][]float64, nw)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make(map[string]map[int64][]float64)
			for i := w; i < len(traces); i += nw {
				for _, span := range traces[i] {
					bucket := timeBucketMinute(span.StartTime)
					dur := float64(span.Duration)
					for _, group := range metadataGroupKeys(span.Feature, span.Metadata) {
						m, ok := local[group]
						if !ok {
							m = make(map[int64][]float64)
							local[group] = m
						}
						m[bucket] = append(m[bucket], dur)
					}
				}
			}
			partials[w] = local
		}(w)
	}
	wg.Wait()

	// Phase 2: merge — concatenate per-group-bucket slices.
	durationByTime := make(map[string]map[int64][]float64)
	for _, local := range partials {
		for group, buckets := range local {
			m, ok := durationByTime[group]
			if !ok {
				m = make(map[int64][]float64)
				durationByTime[group] = m
			}
			for bucket, durs := range buckets {
				m[bucket] = append(m[bucket], durs...)
			}
		}
	}

	percentiles := []int{50, 90, 99}

	// Phase 3: parallel sort + percentile — each worker processes a subset of groups.
	type groupResult struct {
		group      string
		buckets    map[int64]map[string]float64
		totalCount int
	}
	groups := make([]string, 0, len(durationByTime))
	for group := range durationByTime {
		groups = append(groups, group)
	}
	groupResults := make([]groupResult, len(groups))
	wg = sync.WaitGroup{}
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(groups); i += nw {
				group := groups[i]
				buckets := durationByTime[group]
				resultBuckets := make(map[int64]map[string]float64, len(buckets))
				totalCount := 0
				for bucket, durations := range buckets {
					totalCount += len(durations)
					sort.Float64s(durations)
					pMap := make(map[string]float64, len(percentiles))
					for _, p := range percentiles {
						pMap[fmt.Sprintf("p%d", p)] = percentile(durations, float64(p))
					}
					resultBuckets[bucket] = pMap
				}
				groupResults[i] = groupResult{group, resultBuckets, totalCount}
			}
		}(w)
	}
	wg.Wait()

	// Assemble final result
	result := make(map[string]map[int64]map[string]float64, len(groups))
	totalByGroup := make(map[string]int, len(groups))
	for _, gr := range groupResults {
		result[gr.group] = gr.buckets
		totalByGroup[gr.group] = gr.totalCount
	}

	output := map[string]any{
		"duration_percentiles_by_time": result,
		"total_spans_by_group":         totalByGroup,
	}

	return writeEvalResult(dir, "duration_over_time_results.json", output)
}

// percentile computes the p-th percentile of a sorted slice using linear interpolation
// (matching numpy's default method).
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}

	idx := p / 100.0 * float64(n-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))

	if lo == hi {
		return sorted[lo]
	}

	frac := idx - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}
