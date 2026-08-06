package main

import (
	"math"
	"sort"
)

// serviceStats accumulates per-service span statistics for RCA scoring.
type serviceStats struct {
	totalSpans int
	errorSpans int
	durations  []float64
}

// collectServiceStats iterates all spans and groups stats by service name.
// When injectTimeUs is non-nil, only spans starting at or after that time are included.
func collectServiceStats(traces []evalTrace, injectTimeUs *int64) map[string]*serviceStats {
	stats := make(map[string]*serviceStats)
	for _, trace := range traces {
		for _, span := range trace {
			if injectTimeUs != nil && span.StartTime < *injectTimeUs {
				continue
			}
			svc := span.Feature.ServiceName()
			if svc == "" {
				continue
			}
			s, ok := stats[svc]
			if !ok {
				s = &serviceStats{}
				stats[svc] = s
			}
			s.totalSpans++
			s.durations = append(s.durations, float64(span.Duration))

			statusCode := span.Feature.StatusCode()
			if statusCode == "" {
				statusCode = metadataGet(span.Metadata, "status.code")
			}
			if statusCode == "2" {
				s.errorSpans++
			}
		}
	}
	return stats
}

// collectBaselineStats collects stats for spans BEFORE injectTimeUs (normal period).
// Returns nil if injectTimeUs is nil.
func collectBaselineStats(traces []evalTrace, injectTimeUs *int64) map[string]*serviceStats {
	if injectTimeUs == nil {
		return nil
	}
	stats := make(map[string]*serviceStats)
	for _, trace := range traces {
		for _, span := range trace {
			if span.StartTime >= *injectTimeUs {
				continue
			}
			svc := span.Feature.ServiceName()
			if svc == "" {
				continue
			}
			s, ok := stats[svc]
			if !ok {
				s = &serviceStats{}
				stats[svc] = s
			}
			s.totalSpans++
			s.durations = append(s.durations, float64(span.Duration))

			statusCode := span.Feature.StatusCode()
			if statusCode == "" {
				statusCode = metadataGet(span.Metadata, "status.code")
			}
			if statusCode == "2" {
				s.errorSpans++
			}
		}
	}
	return stats
}

// buildServiceCallGraph builds a directed call graph: parent_svc → child_svc → call_count.
// When injectTimeUs is non-nil, only spans starting at or after that time are included.
func buildServiceCallGraph(traces []evalTrace, injectTimeUs *int64) map[string]map[string]float64 {
	graph := make(map[string]map[string]float64)
	for _, trace := range traces {
		for _, span := range trace {
			if injectTimeUs != nil && span.StartTime < *injectTimeUs {
				continue
			}
			if span.ParentSpanID == "" {
				continue
			}
			parent, ok := trace[span.ParentSpanID]
			if !ok {
				continue
			}
			parentSvc := parent.Feature.ServiceName()
			childSvc := span.Feature.ServiceName()
			if parentSvc == "" || childSvc == "" || parentSvc == childSvc {
				continue
			}
			if graph[parentSvc] == nil {
				graph[parentSvc] = make(map[string]float64)
			}
			graph[parentSvc][childSvc]++
		}
	}
	return graph
}

// ksStatistic computes the two-sample Kolmogorov-Smirnov statistic between two
// sorted samples: the maximum absolute difference between their empirical CDFs.
func ksStatistic(a, b []float64) float64 {
	sort.Float64s(a)
	sort.Float64s(b)
	na, nb := float64(len(a)), float64(len(b))
	var i, j int
	var maxD float64
	for i < len(a) && j < len(b) {
		cdfA := float64(i+1) / na
		cdfB := float64(j+1) / nb
		if d := math.Abs(cdfA - cdfB); d > maxD {
			maxD = d
		}
		if a[i] <= b[j] {
			i++
		} else {
			j++
		}
	}
	// Drain remaining
	for i < len(a) {
		cdfA := float64(i+1) / na
		cdfB := float64(j) / nb
		if d := math.Abs(cdfA - cdfB); d > maxD {
			maxD = d
		}
		i++
	}
	for j < len(b) {
		cdfA := float64(i) / na
		cdfB := float64(j+1) / nb
		if d := math.Abs(cdfA - cdfB); d > maxD {
			maxD = d
		}
		j++
	}
	return maxD
}

// anomalyScores computes per-service anomaly scores:
// score = 0.5*errorRate + 0.5*KS(baseline, fault durations)
//
// baselineStats provides normal-period durations for KS comparison.
// If baselineStats is nil or a service has no baseline data, latencyScore is 0.
func anomalyScores(stats map[string]*serviceStats, baselineStats map[string]*serviceStats) map[string]float64 {
	scores := make(map[string]float64, len(stats))
	for svc, s := range stats {
		if s.totalSpans == 0 {
			continue
		}
		errorRate := float64(s.errorSpans) / float64(s.totalSpans)

		var latencyScore float64
		if baselineStats != nil {
			if bs, ok := baselineStats[svc]; ok && len(bs.durations) > 0 {
				latencyScore = ksStatistic(
					append([]float64(nil), s.durations...),
					append([]float64(nil), bs.durations...),
				)
			}
		}

		scores[svc] = 0.5*errorRate + 0.5*latencyScore
	}
	return scores
}

type rankedService struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
}

// evaluateTraceRCA implements spectrum-based fault localization:
// ranks services by combined error rate + latency anomaly rate.
func evaluateTraceRCA(dir string, traces []evalTrace, injectTimeUs *int64) error {
	// Without inject_time, there's no normal-vs-fault comparison possible.
	// Write an empty result to satisfy is_done() checks without expensive iteration.
	if injectTimeUs == nil {
		return writeEvalResult(dir, "trace_rca_results.json", map[string]any{
			"ranks":  []string{},
			"scores": map[string]float64{},
		})
	}

	stats := collectServiceStats(traces, injectTimeUs)
	baselineStats := collectBaselineStats(traces, injectTimeUs)
	scores := anomalyScores(stats, baselineStats)

	ranked := make([]rankedService, 0, len(scores))
	for svc, score := range scores {
		ranked = append(ranked, rankedService{Name: svc, Score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Name < ranked[j].Name
	})

	ranks := make([]string, len(ranked))
	scoreMap := make(map[string]float64, len(ranked))
	for i, r := range ranked {
		ranks[i] = r.Name
		scoreMap[r.Name] = r.Score
	}

	result := map[string]any{
		"ranks":  ranks,
		"scores": scoreMap,
	}
	return writeEvalResult(dir, "trace_rca_results.json", result)
}

// evaluateMicroRank implements personalized PageRank over the service call graph,
// using anomaly scores as the personalization vector.
func evaluateMicroRank(dir string, traces []evalTrace, injectTimeUs *int64) error {
	// Without inject_time, there's no normal-vs-fault comparison possible.
	// Write an empty result to satisfy is_done() checks without expensive iteration.
	if injectTimeUs == nil {
		return writeEvalResult(dir, "micro_rank_results.json", map[string]any{
			"ranks":  []string{},
			"scores": map[string]float64{},
		})
	}

	stats := collectServiceStats(traces, injectTimeUs)
	baselineStats := collectBaselineStats(traces, injectTimeUs)
	scores := anomalyScores(stats, baselineStats)
	callGraph := buildServiceCallGraph(traces, injectTimeUs)

	// Collect all services (nodes)
	serviceSet := make(map[string]bool)
	for svc := range stats {
		serviceSet[svc] = true
	}
	for src := range callGraph {
		serviceSet[src] = true
		for dst := range callGraph[src] {
			serviceSet[dst] = true
		}
	}
	services := make([]string, 0, len(serviceSet))
	for svc := range serviceSet {
		services = append(services, svc)
	}
	sort.Strings(services)
	n := len(services)

	if n == 0 {
		result := map[string]any{
			"ranks":  []string{},
			"scores": map[string]float64{},
		}
		return writeEvalResult(dir, "micro_rank_results.json", result)
	}

	svcIdx := make(map[string]int, n)
	for i, svc := range services {
		svcIdx[svc] = i
	}

	// Build transition matrix T[i][j] = probability of going from i to j.
	// T[i][j] = weight(i→j) / sum(weight(i→*))
	T := make([][]float64, n)
	for i := range T {
		T[i] = make([]float64, n)
	}
	for src, dsts := range callGraph {
		srcI := svcIdx[src]
		total := 0.0
		for _, w := range dsts {
			total += w
		}
		if total > 0 {
			for dst, w := range dsts {
				dstI := svcIdx[dst]
				T[srcI][dstI] = w / total
			}
		}
	}

	// Personalization vector from anomaly scores (normalized).
	p := make([]float64, n)
	pSum := 0.0
	for _, svc := range services {
		pSum += scores[svc]
	}
	if pSum > 0 {
		for i, svc := range services {
			p[i] = scores[svc] / pSum
		}
	} else {
		// Uniform if all scores are zero
		for i := range p {
			p[i] = 1.0 / float64(n)
		}
	}

	// Power iteration: r[j] = (1-d)*p[j] + d*Σ(T[i][j]*r[i])
	d := 0.85
	r := make([]float64, n)
	copy(r, p) // initialize with personalization

	for range 50 {
		rNew := make([]float64, n)
		for j := range n {
			sum := 0.0
			for i := range n {
				sum += T[i][j] * r[i]
			}
			rNew[j] = (1-d)*p[j] + d*sum
		}

		// Check convergence
		diff := 0.0
		for i := range r {
			diff += math.Abs(rNew[i] - r[i])
		}
		r = rNew
		if diff < 1e-6 {
			break
		}
	}

	// Rank by PageRank score (descending)
	type svcScore struct {
		name  string
		score float64
	}
	ranked := make([]svcScore, n)
	for i, svc := range services {
		ranked[i] = svcScore{name: svc, score: r[i]}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].name < ranked[j].name
	})

	ranks := make([]string, n)
	scoreMap := make(map[string]float64, n)
	for i, rs := range ranked {
		ranks[i] = rs.name
		scoreMap[rs.name] = rs.score
	}

	result := map[string]any{
		"ranks":  ranks,
		"scores": scoreMap,
	}
	return writeEvalResult(dir, "micro_rank_results.json", result)
}
