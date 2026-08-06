package main

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// failureRow represents one query's comparison between approach and baseline.
type failureRow struct {
	approach        string
	queryType       string
	percentile      string
	filterKey       string
	filterLevel     int
	filterType      string
	count           int
	cardinality     string
	occurrence      string
	skew            float64
	metaFilterCount int
	metaFilterRatio float64
	avgDepth        float64
	sharedEdgeRate  float64
	approachScore   float64
	baselineScore   float64
	delta           float64
}

// computeMetaFilterStats computes meta_filter_count and meta_filter_ratio from filter_type.
// filter_type is e.g. "F;M" → count=1, ratio=0.5; "-" → count=0, ratio=0.
func computeMetaFilterStats(filterType string) (int, float64) {
	if filterType == "-" {
		return 0, 0.0
	}
	segments := strings.Split(filterType, ";")
	mCount := 0
	for _, s := range segments {
		if s == "M" {
			mCount++
		}
	}
	return mCount, float64(mCount) / float64(len(segments))
}

// buildSpanContextIndex computes depth and ancestor edges for all spans in all traces.
func buildSpanContextIndex(traces []*tpackmodel.Trace) (
	globalEdgeWeights map[string]float64,
	spanDepths map[string]int, // spanID → depth
	spanAncestorEdges map[string][]string, // spanID → list of edge keys to root
) {
	globalEdgeWeights = make(map[string]float64)
	spanDepths = make(map[string]int)
	spanAncestorEdges = make(map[string][]string)

	// Build spanID → tpackmodel.Span lookup
	allSpans := make(map[string]*tpackmodel.Span)
	for _, t := range traces {
		maps.Copy(allSpans, t.Spans)
	}

	// Compute depths via memoized recursion
	var getDepth func(spanID string) int
	getDepth = func(spanID string) int {
		if d, ok := spanDepths[spanID]; ok {
			return d
		}
		s := allSpans[spanID]
		if s == nil || s.ParentSpanID == "" {
			spanDepths[spanID] = 0
			return 0
		}
		d := 1 + getDepth(s.ParentSpanID)
		spanDepths[spanID] = d
		return d
	}

	// Compute ancestor edges (stops at orphan spans whose parent is missing)
	var getAncestorEdges func(spanID string) []string
	getAncestorEdges = func(spanID string) []string {
		if edges, ok := spanAncestorEdges[spanID]; ok {
			return edges
		}
		s := allSpans[spanID]
		if s == nil || s.ParentSpanID == "" {
			spanAncestorEdges[spanID] = nil
			return nil
		}
		parent := allSpans[s.ParentSpanID]
		if parent == nil {
			// Orphan: parent not in data, stop chain here
			spanAncestorEdges[spanID] = nil
			return nil
		}
		edge := string(parent.Feature) + "->" + string(s.Feature)
		parentEdges := getAncestorEdges(s.ParentSpanID)
		edges := make([]string, 0, len(parentEdges)+1)
		edges = append(edges, edge)
		edges = append(edges, parentEdges...)
		spanAncestorEdges[spanID] = edges
		return edges
	}

	for sid := range allSpans {
		getDepth(sid)
		getAncestorEdges(sid)
	}

	// Build global edge weights using ancestor-path counting (same method as filtered weights).
	// For each span, count each unique edge type in its ancestor path once.
	for sid := range allSpans {
		edges := spanAncestorEdges[sid]
		seen := make(map[string]bool)
		for _, e := range edges {
			if !seen[e] {
				globalEdgeWeights[e]++
				seen[e] = true
			}
		}
	}

	return globalEdgeWeights, spanDepths, spanAncestorEdges
}

// spanInvertedIndex maps "col:val" → set of spanIDs for fast predicate matching.
type spanInvertedIndex struct {
	index    map[string]map[string]bool // "col:val" → {spanID: true}
	allSpans map[string]bool            // all spanIDs
}

// buildSpanInvertedIndex creates an inverted index from all spans' merged metadata.
func buildSpanInvertedIndex(traces []*tpackmodel.Trace) *spanInvertedIndex {
	idx := &spanInvertedIndex{
		index:    make(map[string]map[string]bool),
		allSpans: make(map[string]bool),
	}
	addKey := func(sid, col, val string) {
		key := col + ":" + val
		if idx.index[key] == nil {
			idx.index[key] = make(map[string]bool)
		}
		idx.index[key][sid] = true
	}
	for _, t := range traces {
		for sid, s := range t.Spans {
			idx.allSpans[sid] = true
			// Metadata first (wins on key collision with Feature).
			for col, val := range s.Metadata {
				if val != "" {
					addKey(sid, col, val)
				}
			}
			s.Feature.Range(func(col, val string) bool {
				if val == "" {
					return true
				}
				if _, exists := s.Metadata[col]; !exists {
					addKey(sid, col, val)
				}
				return true
			})
		}
	}
	return idx
}

// lookup returns spanIDs matching the group key predicates using inverted index intersection.
func (idx *spanInvertedIndex) lookup(groupKey string) map[string]bool {
	level := filterLevel(groupKey)
	if level == 0 {
		return idx.allSpans
	}
	predicates := parseFilterPredicates(groupKey)
	if len(predicates) == 0 {
		return nil
	}

	// Start with the smallest posting list for efficiency
	var result map[string]bool
	for _, p := range predicates {
		key := p[0] + ":" + p[1]
		posting := idx.index[key]
		if posting == nil {
			return nil // no matches
		}
		if result == nil {
			// Copy first posting list
			result = make(map[string]bool, len(posting))
			for sid := range posting {
				result[sid] = true
			}
		} else {
			// Intersect
			for sid := range result {
				if !posting[sid] {
					delete(result, sid)
				}
			}
		}
		if len(result) == 0 {
			return nil
		}
	}
	return result
}

// computeAvgDepthAndSharedEdgeRate computes avg_depth and shared_edge_rate for a group key.
func computeAvgDepthAndSharedEdgeRate(
	matchingSpans map[string]bool,
	globalEdgeWeights map[string]float64,
	spanDepths map[string]int,
	spanAncestorEdges map[string][]string,
) (float64, float64) {
	if len(matchingSpans) == 0 {
		return -1, -1
	}

	// avg_depth
	totalDepth := 0
	for sid := range matchingSpans {
		totalDepth += spanDepths[sid]
	}
	avgDepth := float64(totalDepth) / float64(len(matchingSpans))

	// shared_edge_rate: sum(w_q(e)) / sum(w_G(e)) for edges in E_q
	filteredEdgeWeights := make(map[string]float64)
	for sid := range matchingSpans {
		edges := spanAncestorEdges[sid]
		seen := make(map[string]bool)
		for _, e := range edges {
			if !seen[e] {
				filteredEdgeWeights[e]++
				seen[e] = true
			}
		}
	}

	if len(filteredEdgeWeights) == 0 {
		// All matching spans are roots or orphans (no ancestor edges).
		// They don't share edges — treat as 0 (dedicated path).
		return avgDepth, 0
	}

	var wqSum, wgSum float64
	for e, wq := range filteredEdgeWeights {
		wqSum += wq
		wgSum += globalEdgeWeights[e]
	}
	if wgSum == 0 {
		return avgDepth, -1
	}
	sharedEdgeRate := 1.0 - wqSum/wgSum

	return avgDepth, sharedEdgeRate
}

// parseFilterPredicates parses "col:val!@#col2:val2" into a list of (column, value) pairs.
func parseFilterPredicates(groupKey string) [][2]string {
	segments := strings.Split(groupKey, "!@#")
	var predicates [][2]string
	for _, seg := range segments {
		before, after, ok := strings.Cut(seg, ":")
		if !ok {
			continue
		}
		predicates = append(predicates, [2]string{before, after})
	}
	return predicates
}

// computeFilterType classifies each segment in a filter key as F (feature) or M (metadata).
// Returns "-" for level-0 keys (all, time_*, operation_*).
func computeFilterType(groupKey string, features map[string]bool) string {
	level := filterLevel(groupKey)
	if level == 0 {
		return "-"
	}
	segments := strings.Split(groupKey, "!@#")
	types := make([]string, len(segments))
	for i, seg := range segments {
		before, _, ok := strings.Cut(seg, ":")
		if !ok {
			types[i] = "?"
			continue
		}
		colName := before
		if features[colName] {
			types[i] = "F"
		} else {
			types[i] = "M"
		}
	}
	return strings.Join(types, ";")
}

func runFailureAnalysis(reportPath string, compressorPrefixes []string, baselinePrefix string, featureSets map[string]map[string]bool, traceInputPath string) error {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("read report: %w", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("parse report: %w", err)
	}

	reports := getMapField(report, "reports")
	if reports == nil {
		return fmt.Errorf("no 'reports' key in report")
	}

	// Load trace data if --trace-input is provided (for avg_depth, shared_edge_rate)
	var traces []*tpackmodel.Trace
	var globalEdgeWeights map[string]float64
	var spanDepths map[string]int
	var spanAncestorEdges map[string][]string
	var spanIdx *spanInvertedIndex

	if traceInputPath != "" {
		log.Printf("Loading trace data from %s ...", traceInputPath)
		// Discover all columns from feature-sets (union of all approaches)
		allColumns := make(map[string]bool)
		for _, cols := range featureSets {
			for col := range cols {
				allColumns[col] = true
			}
		}
		// We need all columns that appear in group keys.
		// Feature columns and metadata columns are both needed for matching.
		// Use the union of feature-sets as feature columns, and discover remaining from group keys.
		var featureCols, metadataCols []string
		groupKeyCols := discoverGroupKeyColumns(reports)
		for col := range groupKeyCols {
			if allColumns[col] {
				featureCols = append(featureCols, col)
			} else {
				metadataCols = append(metadataCols, col)
			}
		}
		sort.Strings(featureCols)
		sort.Strings(metadataCols)
		log.Printf("Feature columns for trace loading: %v", featureCols)
		log.Printf("Metadata columns for trace loading: %v", metadataCols)

		bucketDurationUs := int64(60) * 1_000_000 // 60s buckets (doesn't matter, we flatten)
		buckets, err := readOTLP(traceInputPath, bucketDurationUs, featureCols, metadataCols)
		if err != nil {
			return fmt.Errorf("read trace data: %w", err)
		}
		for _, bucket := range buckets {
			traces = append(traces, bucket...)
		}
		totalSpans := 0
		for _, t := range traces {
			totalSpans += len(t.Spans)
		}
		log.Printf("Loaded %d traces (%d spans)", len(traces), totalSpans)

		globalEdgeWeights, spanDepths, spanAncestorEdges = buildSpanContextIndex(traces)
		log.Printf("Built span context index: %d spans, %d unique edges", len(spanDepths), len(globalEdgeWeights))

		log.Printf("Building inverted index for fast predicate matching ...")
		spanIdx = buildSpanInvertedIndex(traces)
		log.Printf("Inverted index: %d unique col:val keys", len(spanIdx.index))
	}

	// For each approach prefix, find the _1 (run 1) compressor key
	var rows []failureRow

	for _, prefix := range compressorPrefixes {
		approachKey := findCompressorKey(reports, prefix, 1)
		if approachKey == "" {
			log.Printf("Warning: no run-1 key found for prefix %q", prefix)
			continue
		}
		baselineKey := findCompressorKey(reports, baselinePrefix, 1)
		if baselineKey == "" {
			log.Printf("Warning: no run-1 key found for baseline prefix %q", baselinePrefix)
			continue
		}

		approachScores := scoresWithCount(reports, approachKey)
		baselineScores := scoresWithCount(reports, baselineKey)

		// Build cardinality/skew context from approach scores (same data either way)
		cardSkew := computeCardinalitySkew(approachScores)

		totalQueries := len(approachScores)
		processed := 0
		log.Printf("Processing %d queries for %s ...", totalQueries, prefix)

		// Cache expensive per-groupKey computations (shared across percentiles)
		type groupKeyCache struct {
			level           int
			filterType      string
			count           int
			cardinality     string
			occurrence      string
			skew            float64
			metaFilterCount int
			metaFilterRatio float64
			avgDepth        float64
			sharedEdgeRate  float64
		}
		gkCache := make(map[string]*groupKeyCache)

		getGroupKeyCache := func(section, groupKey, query string, count int) *groupKeyCache {
			if c, ok := gkCache[groupKey]; ok {
				return c
			}
			level := filterLevel(groupKey)
			cs := cardSkew[query]
			ft := computeFilterType(groupKey, featureSets[prefix])
			mfCount, mfRatio := computeMetaFilterStats(ft)

			avgDepth := -1.0
			sharedEdgeRate := -1.0
			if spanIdx != nil && (strings.HasPrefix(section, "duration_over_time") || section == "rate_over_time" || section == "error_over_time") {
				matching := spanIdx.lookup(groupKey)
				avgDepth, sharedEdgeRate = computeAvgDepthAndSharedEdgeRate(
					matching, globalEdgeWeights, spanDepths, spanAncestorEdges)
			}

			c := &groupKeyCache{
				level:           level,
				filterType:      ft,
				count:           count,
				cardinality:     cs.cardinality,
				occurrence:      cs.occurrence,
				skew:            cs.skew,
				metaFilterCount: mfCount,
				metaFilterRatio: mfRatio,
				avgDepth:        avgDepth,
				sharedEdgeRate:  sharedEdgeRate,
			}
			gkCache[groupKey] = c
			return c
		}

		for query, as := range approachScores {
			bs, ok := baselineScores[query]
			if !ok {
				processed++
				continue
			}

			section, groupKey := splitQuery(query)
			c := getGroupKeyCache(section, groupKey, query, as.count)

			processed++
			if processed%200 == 0 || processed == totalQueries {
				log.Printf("  %s: %d/%d queries processed", prefix, processed, totalQueries)
			}

			rows = append(rows, failureRow{
				approach:        prefix,
				queryType:       sectionShort(section),
				percentile:      sectionPercentile(section),
				filterKey:       groupKey,
				filterLevel:     c.level,
				filterType:      c.filterType,
				count:           c.count,
				cardinality:     c.cardinality,
				occurrence:      c.occurrence,
				skew:            c.skew,
				metaFilterCount: c.metaFilterCount,
				metaFilterRatio: c.metaFilterRatio,
				avgDepth:        c.avgDepth,
				sharedEdgeRate:  c.sharedEdgeRate,
				approachScore:   as.score,
				baselineScore:   bs.score,
				delta:           as.score - bs.score,
			})
		}
	}

	// Write one file per approach
	outDir := filepath.Dir(reportPath)
	for _, prefix := range compressorPrefixes {
		var prefixRows []failureRow
		for _, r := range rows {
			if r.approach == prefix {
				prefixRows = append(prefixRows, r)
			}
		}
		sort.Slice(prefixRows, func(i, j int) bool { return prefixRows[i].delta < prefixRows[j].delta })

		outPath := filepath.Join(outDir, fmt.Sprintf("failure_analysis_%s.tsv", prefix))
		f, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", outPath, err)
		}

		fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			"approach", "query_type", "percentile", "level", "filter_type", "count", "card", "occur", "skew",
			"meta_filter_count", "meta_filter_ratio", "avg_depth", "shared_edge_rate",
			"approach_score", "baseline_score", "delta", "filter_key")

		for _, r := range prefixRows {
			countStr := fmt.Sprintf("%d", r.count)
			if r.count < 0 {
				countStr = "N/A"
			}
			avgDepthStr := fmt.Sprintf("%.2f", r.avgDepth)
			if r.avgDepth < 0 {
				avgDepthStr = "-1"
			}
			serStr := fmt.Sprintf("%.4f", r.sharedEdgeRate)
			if r.sharedEdgeRate < 0 {
				serStr = "-1"
			}
			fmt.Fprintf(f, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%.2f\t%d\t%.2f\t%s\t%s\t%.2f\t%.2f\t%.2f\t%s\n",
				r.approach, r.queryType, r.percentile, r.filterLevel, r.filterType,
				countStr, r.cardinality, r.occurrence, r.skew,
				r.metaFilterCount, r.metaFilterRatio, avgDepthStr, serStr,
				r.approachScore, r.baselineScore, r.delta, r.filterKey)
		}

		f.Close()
		log.Printf("Wrote %s (%d rows)", outPath, len(prefixRows))
	}
	return nil
}

// discoverGroupKeyColumns extracts all column names used in group keys across the report.
func discoverGroupKeyColumns(reports map[string]any) map[string]bool {
	cols := make(map[string]bool)
	for _, section := range []string{
		"rate_over_time", "error_over_time",
		"duration_over_time_p50", "duration_over_time_p90", "duration_over_time_p99",
	} {
		sectionData := getMapField(reports, section)
		if sectionData == nil {
			continue
		}
		// Iterate over any compressor to find group keys
		for _, compVal := range sectionData {
			compData, ok := compVal.(map[string]any)
			if !ok {
				continue
			}
			for groupKey := range compData {
				if filterLevel(groupKey) == 0 {
					continue
				}
				segments := strings.SplitSeq(groupKey, "!@#")
				for seg := range segments {
					colonIdx := strings.Index(seg, ":")
					if colonIdx > 0 {
						cols[seg[:colonIdx]] = true
					}
				}
			}
			break // only need one compressor to discover columns
		}
	}
	return cols
}

type queryScore struct {
	score float64
	count int
}

type cardSkewInfo struct {
	cardinality string // semicolon-separated per-segment cardinalities, e.g. "100;2"
	occurrence  string // semicolon-separated per-segment occurrence ratios, e.g. "0.10;0.20"
	skew        float64
}

// scoresWithCount extracts per-query fidelity scores and counts from the report.
func scoresWithCount(reports map[string]any, compressorKey string) map[string]queryScore {
	result := make(map[string]queryScore)

	// rate / duration / error over time → mape_fidelity
	for _, section := range []string{
		"rate_over_time", "error_over_time",
		"duration_over_time_p50", "duration_over_time_p90", "duration_over_time_p99",
	} {
		sectionData := getMapField(reports, section)
		if sectionData == nil {
			continue
		}
		compData := getMapField(sectionData, compressorKey)
		if compData == nil {
			continue
		}
		for groupKey, groupVal := range compData {
			gm, ok := groupVal.(map[string]any)
			if !ok {
				continue
			}
			if _, hasMape := gm["mape_fidelity"]; !hasMape {
				continue
			}
			count := int(getFloat(gm, "count"))
			score := getFloat(gm, "mape_fidelity")
			result[section+"::"+groupKey] = queryScore{score: score, count: count}
		}
	}

	// operation → f1 * 100
	opData := getMapField(reports, "operation")
	if opData != nil {
		compOp := getMapField(opData, compressorKey)
		if compOp != nil {
			for _, metric := range []string{"operation_f1", "operation_pair_f1"} {
				metricData := getMapField(compOp, metric)
				if metricData == nil {
					continue
				}
				if _, hasAvg := metricData["avg"]; !hasAvg {
					continue
				}
				result["operation::"+metric] = queryScore{
					score: getFloat(metricData, "avg") * 100,
					count: -1,
				}
			}
		}
	}

	// graph → fidelity per time bucket
	graphData := getMapField(reports, "graph")
	if graphData != nil {
		compGraph := getMapField(graphData, compressorKey)
		for bucketKey, bv := range compGraph {
			if !strings.HasPrefix(bucketKey, "time_") {
				continue
			}
			bm, ok := bv.(map[string]any)
			if !ok {
				continue
			}
			if _, hasFid := bm["fidelity"]; !hasFid {
				continue
			}
			result["graph::"+bucketKey] = queryScore{
				score: getFloat(bm, "fidelity"),
				count: -1,
			}
		}
	}

	return result
}

// findCompressorKey finds a compressor key like "{appName}_{prefix}_{run}" in report sections.
func findCompressorKey(reports map[string]any, prefix string, run int) string {
	suffix := fmt.Sprintf("_%d", run)
	for _, sectionKey := range []string{"duration_over_time_p50", "rate_over_time", "error_over_time", "operation", "graph"} {
		sectionData := getMapField(reports, sectionKey)
		if sectionData == nil {
			continue
		}
		for key := range sectionData {
			// Match: key contains prefix followed by _run
			// e.g. "otel-demo-transformed_tpack_auto_1" matches prefix "tpack_auto" run 1
			if strings.Contains(key, prefix+suffix) {
				return key
			}
		}
	}
	return ""
}

func splitQuery(query string) (section, groupKey string) {
	before, after, ok := strings.Cut(query, "::")
	if !ok {
		return query, ""
	}
	return before, after
}

func filterLevel(groupKey string) int {
	if groupKey == "all" || groupKey == "" {
		return 0
	}
	if strings.HasPrefix(groupKey, "time_") {
		return 0 // graph time buckets
	}
	if strings.HasPrefix(groupKey, "operation_") {
		return 0 // operation metrics
	}
	return strings.Count(groupKey, "!@#") + 1
}

func sectionShort(section string) string {
	if strings.HasPrefix(section, "duration_over_time_p") {
		return "duration"
	}
	switch section {
	case "rate_over_time":
		return "rate"
	case "duration_over_time":
		return "duration"
	case "error_over_time":
		return "error"
	case "operation":
		return "operation"
	case "graph":
		return "graph"
	default:
		return section
	}
}

// sectionPercentile extracts the percentile label from a section name like "duration_over_time_p50".
// Returns "-" for non-percentile sections.
func sectionPercentile(section string) string {
	if strings.HasPrefix(section, "duration_over_time_p") {
		return section[len("duration_over_time_"):]
	}
	return "-"
}

// computeCardinalitySkew computes per-segment cardinality and skew for each query.
// For a level-2 key like "service.name:frontend!@#http.status_code:308", cardinality
// is "10,3" — 10 distinct service.name values overall, 3 distinct http.status_code
// values under service.name:frontend.
func computeCardinalitySkew(scores map[string]queryScore) map[string]cardSkewInfo {
	// Build sibling groups for every (section, prefix) combination.
	// For query section::seg0!@#seg1!@#seg2:
	//   position 0 prefix: "section::colName0:"
	//   position 1 prefix: "section::seg0!@#colName1:"
	//   position 2 prefix: "section::seg0!@#seg1!@#colName2:"
	type siblingGroup struct {
		queries []string
		counts  []float64
	}
	siblingMap := make(map[string]*siblingGroup)

	for query, qs := range scores {
		section, groupKey := splitQuery(query)
		level := filterLevel(groupKey)
		if level == 0 {
			continue
		}

		segments := strings.Split(groupKey, "!@#")
		for i, seg := range segments {
			colonIdx := strings.Index(seg, ":")
			if colonIdx < 0 {
				continue
			}
			colName := seg[:colonIdx+1] // includes ":"

			var prefix string
			if i == 0 {
				prefix = section + "::" + colName
			} else {
				prefix = section + "::" + strings.Join(segments[:i], "!@#") + "!@#" + colName
			}

			if siblingMap[prefix] == nil {
				siblingMap[prefix] = &siblingGroup{}
			}
			sg := siblingMap[prefix]
			sg.queries = append(sg.queries, query)
			count := float64(qs.count)
			if count < 0 {
				count = 1
			}
			sg.counts = append(sg.counts, count)
		}
	}

	// Compute per sibling group: cardinality, per-value count totals, group total, skew
	type groupStats struct {
		cardinality int
		skew        float64
		valCount    map[string]float64 // value → total count
		total       float64            // sum of all value counts
	}
	groupInfo := make(map[string]*groupStats)
	for prefix, sg := range siblingMap {
		valCount := make(map[string]float64)
		for idx, q := range sg.queries {
			_, gk := splitQuery(q)
			segments := strings.Split(gk, "!@#")
			var segVal string
			for i, seg := range segments {
				colonIdx := strings.Index(seg, ":")
				if colonIdx < 0 {
					continue
				}
				var testPrefix string
				if i == 0 {
					testPrefix = splitSection(q) + "::" + seg[:colonIdx+1]
				} else {
					testPrefix = splitSection(q) + "::" + strings.Join(segments[:i], "!@#") + "!@#" + seg[:colonIdx+1]
				}
				if testPrefix == prefix {
					segVal = seg[colonIdx+1:]
					break
				}
			}
			valCount[segVal] += sg.counts[idx]
		}
		var uniqueCounts []float64
		var total float64
		for _, c := range valCount {
			uniqueCounts = append(uniqueCounts, c)
			total += c
		}
		groupInfo[prefix] = &groupStats{
			cardinality: len(valCount),
			skew:        computeSkew(uniqueCounts),
			valCount:    valCount,
			total:       total,
		}
	}

	result := make(map[string]cardSkewInfo, len(scores))

	// Level-0 queries
	for query := range scores {
		_, groupKey := splitQuery(query)
		if filterLevel(groupKey) == 0 {
			result[query] = cardSkewInfo{cardinality: "1", occurrence: "-", skew: 0}
		}
	}

	// Build per-query multi-cardinality and occurrence strings
	for query := range scores {
		section, groupKey := splitQuery(query)
		level := filterLevel(groupKey)
		if level == 0 {
			continue
		}
		segments := strings.Split(groupKey, "!@#")
		cards := make([]string, len(segments))
		occs := make([]string, len(segments))
		var lastSkew float64
		for i, seg := range segments {
			colonIdx := strings.Index(seg, ":")
			if colonIdx < 0 {
				cards[i] = "?"
				occs[i] = "?"
				continue
			}
			var prefix string
			if i == 0 {
				prefix = section + "::" + seg[:colonIdx+1]
			} else {
				prefix = section + "::" + strings.Join(segments[:i], "!@#") + "!@#" + seg[:colonIdx+1]
			}
			gs := groupInfo[prefix]
			cards[i] = fmt.Sprintf("%d", gs.cardinality)
			lastSkew = gs.skew
			segVal := seg[colonIdx+1:]
			if gs.total > 0 {
				occs[i] = fmt.Sprintf("%.2f", gs.valCount[segVal]/gs.total)
			} else {
				occs[i] = "0.00"
			}
		}
		result[query] = cardSkewInfo{
			cardinality: strings.Join(cards, ";"),
			occurrence:  strings.Join(occs, ";"),
			skew:        lastSkew,
		}
	}

	return result
}

// splitSection returns the section part of a "section::groupKey" query string.
func splitSection(query string) string {
	s, _ := splitQuery(query)
	return s
}

// computeSkew returns 1 - H/log(C) where H is the Shannon entropy of the distribution.
func computeSkew(counts []float64) float64 {
	if len(counts) <= 1 {
		return 1.0
	}
	total := 0.0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	entropy := 0.0
	for _, c := range counts {
		if c > 0 {
			p := c / total
			entropy -= p * math.Log(p)
		}
	}
	maxEntropy := math.Log(float64(len(counts)))
	if maxEntropy == 0 {
		return 1.0
	}
	return 1.0 - entropy/maxEntropy
}
