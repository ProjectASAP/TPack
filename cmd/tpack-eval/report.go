package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// generateReport produces a JSON report matching the Python generate_report output.
// Two output layouts are supported:
//   - {rootDir}/{app}/{service}/{run}/report.json   — original; needed for RE2 RCA
//   - {rootDir}/{app}/report.json                    — flat layout for non-RCA datasets
//
// Layout is detected from whether the leaf directory name parses as an integer.
func generateReport(outputPath string, compressorPrefixes []string) error {
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("abs path: %w", err)
	}
	leafDir := filepath.Dir(absOut)
	leafName := filepath.Base(leafDir)

	var (
		baseDir string
		appName string
		service string
		run     int
	)
	if n, err := strconv.Atoi(leafName); err == nil {
		// Layered layout: rootDir/app/service/run/report.json
		run = n
		serviceDir := filepath.Dir(leafDir)
		service = filepath.Base(serviceDir)
		appName = filepath.Base(filepath.Dir(serviceDir))
		baseDir = leafDir
	} else {
		// Flat layout: rootDir/app/report.json
		run = 1
		service = ""
		appName = leafName
		baseDir = leafDir
	}

	rcaAnswer := extractRCAAnswer(service)
	log.Printf("Report: app=%s service=%s run=%d baseDir=%s rcaAnswer=%s", appName, service, run, baseDir, rcaAnswer)

	// Expand compressor prefixes
	compressors := expandCompressorPrefixes(compressorPrefixes, baseDir)
	if len(compressors) == 0 {
		return fmt.Errorf("no compressors found matching prefixes %v in %s", compressorPrefixes, baseDir)
	}
	log.Printf("Expanded compressors: %v", compressors)

	reportTypes := []string{
		"duration_over_time_p50", "duration_over_time_p90", "duration_over_time_p99",
		"rate_over_time", "error_over_time",
		"graph", "graph_binary", "span_count", "size", "time",
		"trace_rca", "micro_rank", "anomaly_detection",
	}

	reports := make(map[string]any)
	for _, rt := range reportTypes {
		var result map[string]any
		switch rt {
		case "duration_over_time_p50":
			result = reportDurationOverTime(baseDir, appName, compressors, "p50")
		case "duration_over_time_p90":
			result = reportDurationOverTime(baseDir, appName, compressors, "p90")
		case "duration_over_time_p99":
			result = reportDurationOverTime(baseDir, appName, compressors, "p99")
		case "rate_over_time":
			result = reportCountOverTime(baseDir, appName, compressors, "rate_over_time_results.json", true)
		case "error_over_time":
			result = reportCountOverTime(baseDir, appName, compressors, "error_over_time_results.json", false)
		case "graph":
			result = reportGraphImpl(baseDir, appName, compressors, false)
		case "graph_binary":
			result = reportGraphImpl(baseDir, appName, compressors, true)
		case "span_count":
			result = reportSpanCount(baseDir, appName, compressors)
		case "size":
			result = reportSize(baseDir, appName, compressors)
		case "time":
			result = reportTime(baseDir, appName, compressors)
		case "trace_rca":
			result = reportRCA(baseDir, appName, rcaAnswer, compressors, "trace_rca_results.json")
		case "micro_rank":
			result = reportRCA(baseDir, appName, rcaAnswer, compressors, "micro_rank_results.json")
		case "anomaly_detection":
			result = reportAnomalyDetection(baseDir, appName, rcaAnswer, compressors)
		}
		if result == nil {
			result = make(map[string]any)
		}
		reports[rt] = result
		log.Printf("  %s: %d compressor entries", rt, len(result))
	}

	output := map[string]any{
		"metadata": map[string]any{
			"generator":    "TPack Report Generator",
			"report_types": reportTypes,
		},
		"reports": reports,
	}

	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(absOut, data, 0o644); err != nil {
		return err
	}
	log.Printf("Report written to %s", absOut)
	return nil
}

// expandCompressorPrefixes lists subdirs in baseDir matching any prefix.
func expandCompressorPrefixes(prefixes []string, baseDir string) []string {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	var matched []string
	seen := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		for _, prefix := range prefixes {
			if name == prefix || strings.HasPrefix(name, prefix+"_") {
				if !seen[name] {
					seen[name] = true
					matched = append(matched, name)
				}
			}
		}
	}
	sort.Strings(matched)
	return matched
}

// --- Metrics ---

func symmetricMAPE(orig, res float64) float64 {
	if orig > 0 {
		return math.Abs((orig-res)/(orig+res)) * 100
	}
	if res == 0 {
		return 0
	}
	return 100
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) == 1 {
		if a[0] == b[0] {
			return 1
		}
		return 0
	}
	dot, normA, normB := 0.0, 0.0, 0.0
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// wassersteinDistance computes the 1D Earth Mover's Distance between two samples.
func wassersteinDistance(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	sa := make([]float64, len(a))
	sb := make([]float64, len(b))
	copy(sa, a)
	copy(sb, b)
	sort.Float64s(sa)
	sort.Float64s(sb)

	// Merge all values and compute areas between CDFs
	na, nb := float64(len(sa)), float64(len(sb))
	ia, ib := 0, 0
	cdfA, cdfB := 0.0, 0.0
	prev := math.Min(sa[0], sb[0])
	totalArea := 0.0

	for ia < len(sa) || ib < len(sb) {
		var cur float64
		if ia >= len(sa) {
			cur = sb[ib]
		} else if ib >= len(sb) {
			cur = sa[ia]
		} else if sa[ia] <= sb[ib] {
			cur = sa[ia]
		} else {
			cur = sb[ib]
		}

		totalArea += math.Abs(cdfA-cdfB) * (cur - prev)
		prev = cur

		// Advance all matching values
		for ia < len(sa) && sa[ia] == cur {
			cdfA += 1.0 / na
			ia++
		}
		for ib < len(sb) && sb[ib] == cur {
			cdfB += 1.0 / nb
			ib++
		}
	}
	return totalArea
}

// samplingScale extracts the sampling rate from compressor names like "head_100_1" or "sifter_100_1".
// Handles app-prefixed names like "RE2-TT_head_100_1" or "otel-demo-transformed_sifter_100_1".
// Returns the rate (e.g. 100) for scaling count-based metrics, or 1 if not a sampling compressor.
func samplingScale(compressor string) int {
	// Find "head_" or "sifter_" anywhere in the name, then parse the rate after it
	for _, prefix := range []string{"_head_", "_sifter_"} {
		if _, after, ok := strings.Cut(compressor, prefix); ok {
			after := after
			parts := strings.SplitN(after, "_", 2)
			if n, err := strconv.Atoi(parts[0]); err == nil && n > 0 {
				return n
			}
		}
	}
	// Also check if it starts with head_ or sifter_ (no app prefix)
	for _, prefix := range []string{"head_", "sifter_"} {
		if strings.HasPrefix(compressor, prefix) {
			parts := strings.SplitN(compressor[len(prefix):], "_", 2)
			if n, err := strconv.Atoi(parts[0]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// --- JSON helpers ---

func loadJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func getMapField(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
	}
	return nil
}

func getFloat(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case json.Number:
			f, _ := n.Float64()
			return f
		}
	}
	return 0
}

func getStringSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func getFloatSlice(m map[string]any, key string) []float64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]float64, 0, len(arr))
	for _, item := range arr {
		switch n := item.(type) {
		case float64:
			result = append(result, n)
		case json.Number:
			f, _ := n.Float64()
			result = append(result, f)
		}
	}
	return result
}

// --- Report: duration_over_time ---

func reportDurationOverTime(baseDir, appName string, compressors []string, pct string) map[string]any {
	result := make(map[string]any)

	origPath := filepath.Join(baseDir, "head_1_1", "evaluated", "duration_over_time_results.json")
	original, err := loadJSON(origPath)
	if err != nil {
		log.Printf("  duration_over_time_%s: cannot load baseline %s: %v", pct, origPath, err)
		return result
	}

	origPercentiles := getMapField(original, "duration_percentiles_by_time")
	origTotals := getMapField(original, "total_spans_by_group")

	for _, comp := range compressors {
		compPath := filepath.Join(baseDir, comp, "evaluated", "duration_over_time_results.json")
		compData, err := loadJSON(compPath)
		if err != nil {
			continue
		}
		compPercentiles := getMapField(compData, "duration_percentiles_by_time")

		reportGroup := appName + "_" + comp
		groupResult := make(map[string]any)

		for groupKey, origGroupVal := range origPercentiles {
			origGroup, ok := origGroupVal.(map[string]any)
			if !ok {
				continue
			}

			count := 0.0
			if origTotals != nil {
				count = getFloat(origTotals, groupKey)
			}

			compGroupVal, ok := compPercentiles[groupKey]
			if !ok {
				groupResult[groupKey] = map[string]any{
					"mape_fidelity":   0.0,
					"cosine_fidelity": 0.0,
					"count":           count,
				}
				continue
			}
			compGroup, ok := compGroupVal.(map[string]any)
			if !ok {
				continue
			}

			commonBuckets := commonKeys(origGroup, compGroup)
			if len(commonBuckets) == 0 {
				groupResult[groupKey] = map[string]any{
					"mape_fidelity":   0.0,
					"cosine_fidelity": 0.0,
					"count":           count,
				}
				continue
			}

			var origTS, compTS []float64
			for _, tb := range commonBuckets {
				origBucket, ok1 := origGroup[tb].(map[string]any)
				compBucket, ok2 := compGroup[tb].(map[string]any)
				if !ok1 || !ok2 {
					continue
				}
				origVal, ok1 := origBucket[pct]
				compVal, ok2 := compBucket[pct]
				if !ok1 || !ok2 {
					continue
				}
				origF, ok1 := toFloat(origVal)
				compF, ok2 := toFloat(compVal)
				if !ok1 || !ok2 {
					continue
				}
				origTS = append(origTS, origF)
				compTS = append(compTS, compF)
			}

			if len(origTS) == 0 {
				groupResult[groupKey] = map[string]any{
					"mape_fidelity":   0.0,
					"cosine_fidelity": 0.0,
					"count":           count,
				}
				continue
			}

			mape := avgMAPE(origTS, compTS)
			cosine := cosineSimilarity(origTS, compTS)

			groupResult[groupKey] = map[string]any{
				"mape_fidelity":   math.Max(0, 100-mape),
				"cosine_fidelity": cosine * 100,
				"count":           count,
			}
		}

		if len(groupResult) > 0 {
			result[reportGroup] = groupResult
		}
	}
	return result
}

// --- Report: rate_over_time / error_over_time ---

func reportCountOverTime(baseDir, appName string, compressors []string, filename string, applyScale bool) map[string]any {
	result := make(map[string]any)

	origPath := filepath.Join(baseDir, "head_1_1", "evaluated", filename)
	original, err := loadJSON(origPath)
	if err != nil {
		log.Printf("  %s: cannot load baseline: %v", filename, err)
		return result
	}

	origRate := getMapField(original, "span_rate_by_time")
	origTotals := getMapField(original, "total_spans_by_group")

	for _, comp := range compressors {
		compPath := filepath.Join(baseDir, comp, "evaluated", filename)
		compData, err := loadJSON(compPath)
		if err != nil {
			continue
		}
		compRate := getMapField(compData, "span_rate_by_time")

		reportGroup := appName + "_" + comp
		groupResult := make(map[string]any)
		scaleFactor := 1.0
		if applyScale {
			scaleFactor = float64(samplingScale(comp))
		}

		for groupKey, origGroupVal := range origRate {
			origGroup, ok := origGroupVal.(map[string]any)
			if !ok {
				continue
			}

			count := 0.0
			if origTotals != nil {
				count = getFloat(origTotals, groupKey)
			}

			compGroupVal, ok := compRate[groupKey]
			if !ok {
				// Group missing from compressor → worst-case
				groupResult[groupKey] = map[string]any{
					"mape_fidelity":   0.0,
					"cosine_fidelity": 0.0,
					"count":           count,
				}
				continue
			}
			compGroup, ok := compGroupVal.(map[string]any)
			if !ok {
				continue
			}

			commonBuckets := commonKeys(origGroup, compGroup)
			if len(commonBuckets) == 0 {
				groupResult[groupKey] = map[string]any{
					"mape_fidelity":   0.0,
					"cosine_fidelity": 0.0,
					"count":           count,
				}
				continue
			}

			var origTS, compTS []float64
			for _, tb := range commonBuckets {
				origF, ok1 := toFloat(origGroup[tb])
				compF, ok2 := toFloat(compGroup[tb])
				if !ok1 || !ok2 {
					continue
				}
				origTS = append(origTS, origF)
				compTS = append(compTS, compF*scaleFactor)
			}

			if len(origTS) == 0 {
				groupResult[groupKey] = map[string]any{
					"mape_fidelity":   0.0,
					"cosine_fidelity": 0.0,
					"count":           count,
				}
				continue
			}

			mape := avgMAPE(origTS, compTS)
			cosine := cosineSimilarity(origTS, compTS)

			groupResult[groupKey] = map[string]any{
				"mape_fidelity":   math.Max(0, 100-mape),
				"cosine_fidelity": cosine * 100,
				"count":           count,
			}
		}

		if len(groupResult) > 0 {
			result[reportGroup] = groupResult
		}
	}
	return result
}

// --- Report: graph / graph_binary ---

type graphData struct {
	nodes []string
	edges map[string]float64 // "A->B" -> weight
}

func parseGraphData(m map[string]any) graphData {
	g := graphData{edges: make(map[string]float64)}
	g.nodes = getStringSlice(m, "nodes")
	if edgesRaw := getMapField(m, "edges"); edgesRaw != nil {
		for k, v := range edgesRaw {
			if f, ok := toFloat(v); ok {
				g.edges[k] = f
			}
		}
	}
	return g
}

func graphEditDistance(g1, g2 graphData) float64 {
	// Node costs
	nodeSet1 := make(map[string]bool, len(g1.nodes))
	for _, n := range g1.nodes {
		nodeSet1[n] = true
	}
	nodeSet2 := make(map[string]bool, len(g2.nodes))
	for _, n := range g2.nodes {
		nodeSet2[n] = true
	}

	dist := 0.0
	// Node deletions (in g1 but not g2)
	for n := range nodeSet1 {
		if !nodeSet2[n] {
			dist += 1
		}
	}
	// Node insertions (in g2 but not g1)
	for n := range nodeSet2 {
		if !nodeSet1[n] {
			dist += 1
		}
	}

	// Edge costs - collect all edge keys
	allEdges := make(map[string]bool)
	for k := range g1.edges {
		allEdges[k] = true
	}
	for k := range g2.edges {
		allEdges[k] = true
	}

	for edge := range allEdges {
		w1, in1 := g1.edges[edge]
		w2, in2 := g2.edges[edge]
		if in1 && in2 {
			// Edge substitution: weight difference
			dist += math.Abs(w1 - w2)
		} else if in1 {
			// Edge deletion
			dist += w1
		} else {
			// Edge insertion
			dist += w2
		}
	}

	return dist
}

// reportGraphImpl computes graph fidelity via edit distance.
// When binary is true, all edge weights are clamped to 1 (structural existence only, no scaling).
// When binary is false, edge weights are raw call counts (scaled by sampling rate).
func reportGraphImpl(baseDir, appName string, compressors []string, binary bool) map[string]any {
	result := make(map[string]any)

	label := "graph"
	if binary {
		label = "graph_binary"
	}

	origPath := filepath.Join(baseDir, "head_1_1", "evaluated", "graph_results.json")
	original, err := loadJSON(origPath)
	if err != nil {
		log.Printf("  %s: cannot load baseline: %v", label, err)
		return result
	}
	origGraphs := getMapField(original, "service_graph_by_time")

	for _, comp := range compressors {
		compPath := filepath.Join(baseDir, comp, "evaluated", "graph_results.json")
		compData, err := loadJSON(compPath)
		if err != nil {
			continue
		}
		compGraphs := getMapField(compData, "service_graph_by_time")

		var graphScale float64 = 1.0
		if !binary {
			graphScale = float64(samplingScale(comp))
		}

		reportGroup := appName + "_" + comp

		// Accumulate across runs for averaging
		type bucketAccum struct {
			distances  []float64
			fidelities []float64
		}
		buckets := make(map[string]*bucketAccum)

		for timeBucket, origVal := range origGraphs {
			compVal, ok := compGraphs[timeBucket]
			if !ok {
				// Missing bucket → 0% fidelity
				key := "time_" + timeBucket
				if buckets[key] == nil {
					buckets[key] = &bucketAccum{}
				}
				buckets[key].distances = append(buckets[key].distances, 0)
				buckets[key].fidelities = append(buckets[key].fidelities, 0)
				continue
			}
			origMap, ok := origVal.(map[string]any)
			if !ok {
				continue
			}
			compMap, ok := compVal.(map[string]any)
			if !ok {
				continue
			}

			g1 := parseGraphData(origMap)
			g2 := parseGraphData(compMap)

			// Scale edge weights for sampling approaches
			if graphScale > 1 {
				for k, v := range g2.edges {
					g2.edges[k] = v * graphScale
				}
			}

			// For binary mode, clamp all edge weights to 1
			if binary {
				for k := range g1.edges {
					g1.edges[k] = 1
				}
				for k := range g2.edges {
					g2.edges[k] = 1
				}
			}

			distance := graphEditDistance(g1, g2)

			// Fidelity: reference_size = nodes_g1 + sum(edges_g1) (baseline-relative)
			totalEdgeWeight := 0.0
			for _, w := range g1.edges {
				totalEdgeWeight += w
			}
			refSize := float64(len(g1.nodes)) + totalEdgeWeight
			fidelity := 100.0
			if refSize > 0 {
				fidelity = math.Max(0, 100-distance/refSize*100)
			}

			key := "time_" + timeBucket
			if buckets[key] == nil {
				buckets[key] = &bucketAccum{}
			}
			buckets[key].distances = append(buckets[key].distances, distance)
			buckets[key].fidelities = append(buckets[key].fidelities, fidelity)
		}

		if len(buckets) > 0 {
			groupResult := make(map[string]any)
			for key, acc := range buckets {
				groupResult[key] = map[string]any{
					"avg":      mean(acc.distances),
					"fidelity": mean(acc.fidelities),
				}
			}
			result[reportGroup] = groupResult
		}
	}
	return result
}

// --- Report: span_count ---

func reportSpanCount(baseDir, appName string, compressors []string) map[string]any {
	result := make(map[string]any)

	origPath := filepath.Join(baseDir, "head_1_1", "evaluated", "span_count_results.json")
	original, err := loadJSON(origPath)
	if err != nil {
		log.Printf("  span_count: cannot load baseline: %v", err)
		return result
	}

	for _, comp := range compressors {
		compPath := filepath.Join(baseDir, comp, "evaluated", "span_count_results.json")
		compData, err := loadJSON(compPath)
		if err != nil {
			continue
		}

		reportGroup := appName + "_" + comp
		origCounts := getMapField(original, "span_count")
		compCounts := getMapField(compData, "span_count")

		var wdistValues []float64
		for group := range origCounts {
			if _, ok := compCounts[group]; !ok {
				continue
			}
			origArr := getFloatSlice(origCounts, group)
			compArr := getFloatSlice(compCounts, group)
			if len(origArr) > 0 && len(compArr) > 0 {
				wdist := wassersteinDistance(origArr, compArr)
				wdistValues = append(wdistValues, wdist)
			}
		}

		if len(wdistValues) > 0 {
			result[reportGroup] = map[string]any{
				"span_count_wdist": map[string]any{
					"avg": mean(wdistValues),
				},
			}
		}
	}
	return result
}

// --- Report: size ---

func reportSize(baseDir, appName string, compressors []string) map[string]any {
	result := make(map[string]any)

	for _, comp := range compressors {
		dataDir := filepath.Join(baseDir, comp, "compressed", "data")
		info, err := os.Stat(dataDir)
		if err != nil || !info.IsDir() {
			continue
		}

		totalSize := int64(0)
		filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() {
				totalSize += info.Size()
			}
			return nil
		})

		reportGroup := appName + "_" + comp
		result[reportGroup] = map[string]any{
			"size": map[string]any{
				"avg": totalSize,
			},
		}
	}
	return result
}

// --- Report: time ---

func reportTime(baseDir, appName string, compressors []string) map[string]any {
	result := make(map[string]any)

	for _, comp := range compressors {
		reportGroup := appName + "_" + comp
		groupResult := make(map[string]any)

		compressedDir := filepath.Join(baseDir, comp, "compressed", "data")
		for _, field := range []string{
			"compression_cpu_time_seconds", "compression_gpu_time_seconds",
			"decompression_cpu_time_seconds", "decompression_gpu_time_seconds",
		} {
			data, err := os.ReadFile(filepath.Join(compressedDir, field))
			if err == nil {
				if f, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
					groupResult[field] = map[string]any{"avg": f}
				}
			}
		}

		if len(groupResult) > 0 {
			result[reportGroup] = groupResult
		}
	}
	return result
}

// --- Report: RCA (trace_rca / micro_rank) ---

// knownFaults are fault types used in RE2-OB/RE2-TT fault injection experiments.
var knownFaults = []string{"cpu", "delay", "disk", "loss", "mem", "socket"}

// extractRCAAnswer extracts the ground truth service name from the directory's
// service component. For fault-injection datasets (e.g. "checkoutservice_cpu"),
// strips the known fault suffix to get "checkoutservice".
func extractRCAAnswer(service string) string {
	for _, fault := range knownFaults {
		suffix := "_" + fault
		if strings.HasSuffix(service, suffix) {
			return service[:len(service)-len(suffix)]
		}
	}
	return service
}

func reportRCA(baseDir, appName, rcaAnswer string, compressors []string, filename string) map[string]any {
	result := make(map[string]any)

	for _, comp := range compressors {
		rcaPath := filepath.Join(baseDir, comp, "evaluated", filename)
		rcaData, err := loadJSON(rcaPath)
		if err != nil {
			continue
		}

		ranksRaw, ok := rcaData["ranks"]
		if !ok {
			continue
		}
		ranksArr, ok := ranksRaw.([]any)
		if !ok {
			continue
		}
		ranks := make([]string, 0, len(ranksArr))
		for _, r := range ranksArr {
			if s, ok := r.(string); ok {
				ranks = append(ranks, s)
			}
		}

		reportGroup := appName + "_" + comp

		// Existing accumulator or new one
		existing, ok := result[reportGroup]
		var accum map[string]*struct{ values []float64 }
		if ok {
			accum = existing.(map[string]*struct{ values []float64 })
		} else {
			accum = make(map[string]*struct{ values []float64 })
			for k := 1; k <= 5; k++ {
				accum[fmt.Sprintf("ac%d", k)] = &struct{ values []float64 }{}
			}
			result[reportGroup] = accum
		}

		for k := 1; k <= 5; k++ {
			hit := 0.0
			if acAtK(rcaAnswer, ranks, k) {
				hit = 1.0
			}
			key := fmt.Sprintf("ac%d", k)
			accum[key].values = append(accum[key].values, hit)
		}
	}

	// Finalize: compute averages and ac5
	final := make(map[string]any)
	for group, accumRaw := range result {
		accum := accumRaw.(map[string]*struct{ values []float64 })
		groupResult := make(map[string]any)
		for k := 1; k <= 5; k++ {
			key := fmt.Sprintf("ac%d", k)
			avg := mean(accum[key].values)
			groupResult[key] = map[string]any{"avg": avg}
		}
		// ac5 is the primary metric
		if acc, ok := accum["ac5"]; ok && len(acc.values) > 0 {
			groupResult["ac5_fidelity"] = map[string]any{"avg": mean(acc.values) * 100}
		}
		final[group] = groupResult
	}
	return final
}

// reportAnomalyDetection aggregates per-compressor CUSUM anomaly-detection
// results. For each compressor in baseDir, reads anomaly_detection_results.json
// from its evaluated/ subdir and computes:
//   - detected (1 if first alarm bucket >= inject_bucket - 1, else 0;
//     -1 boundary tolerance accounts for the inject-time-bucket dilution effect)
//   - detection_delay_buckets (first_alarm_bucket - inject_bucket; only present
//     when detected = 1)
//   - false_alarms_pre_inject (count of (svc, bucket) alarms with
//     bucket < inject_bucket - 1)
//   - false_alarm_rate (false_alarms_pre_inject / pre_inject_buckets)
//   - localized_ac{1,3,5} (1 if rcaAnswer ∈ ranked_services[:k], else 0)
//
// All metrics emitted in the {"avg": x} shape that reportRCA uses, so the
// existing Python aggregation plumbing extends with no changes.
func reportAnomalyDetection(baseDir, appName, rcaAnswer string, compressors []string) map[string]any {
	result := make(map[string]any)

	for _, comp := range compressors {
		path := filepath.Join(baseDir, comp, "evaluated", "anomaly_detection_results.json")
		data, err := loadJSON(path)
		if err != nil {
			continue
		}

		var firstAlarmBucket *int
		if v, ok := data["first_alarm_bucket"]; ok && v != nil {
			if f, ok2 := toFloat(v); ok2 {
				ib := int(f)
				firstAlarmBucket = &ib
			}
		}
		var injectBucket *int
		if v, ok := data["inject_bucket"]; ok && v != nil {
			if f, ok2 := toFloat(v); ok2 {
				ib := int(f)
				injectBucket = &ib
			}
		}
		var numBuckets int
		if v, ok := data["num_buckets"]; ok {
			if f, ok2 := toFloat(v); ok2 {
				numBuckets = int(f)
			}
		}

		var rankedServices []string
		if v, ok := data["ranked_services"]; ok {
			if arr, ok2 := v.([]any); ok2 {
				for _, r := range arr {
					if s, ok3 := r.(string); ok3 {
						rankedServices = append(rankedServices, s)
					}
				}
			}
		}

		// Count alarms before inject_bucket-1.
		var falseAlarmsPre int
		if injectBucket != nil {
			if alarms, ok := data["alarms"].(map[string]any); ok {
				for _, svcAlarms := range alarms {
					arr, ok := svcAlarms.([]any)
					if !ok {
						continue
					}
					for _, a := range arr {
						amap, ok := a.(map[string]any)
						if !ok {
							continue
						}
						if b, ok := toFloat(amap["bucket"]); ok && int(b) < *injectBucket-1 {
							falseAlarmsPre++
						}
					}
				}
			}
		}

		groupResult := make(map[string]any)

		detected := 0.0
		if firstAlarmBucket != nil && injectBucket != nil && *firstAlarmBucket >= *injectBucket-1 {
			detected = 1.0
		}
		groupResult["detected"] = map[string]any{"avg": detected}

		if detected == 1.0 {
			delay := float64(*firstAlarmBucket - *injectBucket)
			groupResult["detection_delay_buckets"] = map[string]any{"avg": delay}
		}

		preInjectBuckets := 0
		if injectBucket != nil && *injectBucket > 0 {
			preInjectBuckets = *injectBucket
		}
		groupResult["false_alarms_pre_inject"] = map[string]any{"avg": float64(falseAlarmsPre)}
		groupResult["pre_inject_buckets"] = map[string]any{"avg": float64(preInjectBuckets)}
		fpr := 0.0
		if preInjectBuckets > 0 {
			fpr = float64(falseAlarmsPre) / float64(preInjectBuckets)
		}
		groupResult["false_alarm_rate"] = map[string]any{"avg": fpr}

		for k := 1; k <= 5; k++ {
			hit := 0.0
			if acAtK(rcaAnswer, rankedServices, k) {
				hit = 1.0
			}
			groupResult[fmt.Sprintf("localized_ac%d", k)] = map[string]any{"avg": hit}
		}

		groupResult["num_buckets"] = map[string]any{"avg": float64(numBuckets)}

		result[appName+"_"+comp] = groupResult
	}

	return result
}

func acAtK(answer string, ranks []string, k int) bool {
	if k > len(ranks) {
		k = len(ranks)
	}
	return slices.Contains(ranks[:k], answer)
}

// --- Utility helpers ---

func commonKeys(a, b map[string]any) []string {
	var keys []string
	for k := range a {
		if _, ok := b[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func avgMAPE(orig, res []float64) float64 {
	if len(orig) == 0 {
		return 100
	}
	sum := 0.0
	for i := range orig {
		sum += symmetricMAPE(orig[i], res[i])
	}
	return sum / float64(len(orig))
}
