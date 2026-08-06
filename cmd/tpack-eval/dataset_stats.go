package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// statsResult is the common result returned by all stat functions.
type statsResult struct {
	Format            string  `json:"format"`
	Spans             int     `json:"spans"`
	Traces            int     `json:"traces"`
	Services          int     `json:"services"`
	TotalBytes        int64   `json:"total_bytes"`
	TimeWindowSeconds float64 `json:"time_window_seconds"`

	// Insight 1: Topology repetitiveness
	TotalParentTypes   int     `json:"total_parent_types"`
	LeafCount          int     `json:"leaf_count"`
	LeafPct            float64 `json:"leaf_pct"`
	SingleRelayCount   int     `json:"single_relay_count"`
	SingleRelayPct     float64 `json:"single_relay_pct"`
	VariableRelayCount int     `json:"variable_relay_count"`
	MultiChildPct      float64 `json:"multi_child_pct"`
	TopSetDedupCount   int     `json:"top_set_dedup_count"`
	TopSetDedupPct     float64 `json:"top_set_dedup_pct"`
	TopSetCountsCount  int     `json:"top_set_counts_count"`
	TopSetCountsPct    float64 `json:"top_set_counts_pct"`

	// Insight 2: Span loss
	OrphanSpanPct    float64 `json:"orphan_span_pct"`
	CompleteTracePct float64 `json:"complete_trace_pct"`

	// Trace template stats
	UniqueTemplates    int         `json:"unique_templates"`
	TemplateTraces     int         `json:"template_traces"`
	TemplateSizeHist   map[int]int `json:"template_size_hist,omitempty"`
	TemplateCountsDesc []int       `json:"template_counts_desc,omitempty"` // per-template trace count, sorted desc

	// Edge-probability stats: cardinality of the set of distinct
	// (parent_feature, child_position, child_feature) triples observed.
	UniqueEdges int `json:"unique_edges"`

	// Span attributes
	Attributes []attrStatEntry `json:"attributes,omitempty"`
}

// attrStatEntry holds per-attribute cardinality and coverage.
type attrStatEntry struct {
	Name        string  `json:"name"`
	Cardinality int     `json:"cardinality"`
	Count       int     `json:"count"`
	Coverage    float64 `json:"coverage"`
}

// parentSetCounter tracks children-set frequencies compactly for one parent feature.
type parentSetCounter struct {
	total      int
	leafExecs  int            // executions where this parent had 0 children
	dedupFreq  map[string]int // dedupSet → count (only non-leaf executions)
	countsFreq map[string]int // countsSet → count (only non-leaf executions)
}

// insightAccumulator collects insight metrics incrementally, one trace at a time.
// Not thread-safe — use one per worker and merge() at the end.
type insightAccumulator struct {
	totalSpans     int
	orphanSpans    int
	totalTraces    int
	completeTraces int

	// parentFeature → compact frequency counter
	parentCounters map[string]*parentSetCounter

	// Trace template hashes → count. Hash is FNV-64 of a canonical BFS
	// serialization using feature strings — encoder-free and fixed-size so
	// map lookups/compares are O(1) even for huge templates. Collision
	// probability on 10^6 unique templates is ~10^-8.
	templateHashes map[uint64]int
	templateSizes  map[uint64]int

	// edgeKeys is the set of distinct (parent_feature, child_pos, child_feature)
	// triples, matching the edge key used by the edge probability model
	// (see pkg/tpackmodel/topology_model.go).
	edgeKeys map[string]struct{}
}

func newInsightAccumulator() *insightAccumulator {
	return &insightAccumulator{
		parentCounters: make(map[string]*parentSetCounter),
		templateHashes: make(map[uint64]int),
		templateSizes:  make(map[uint64]int),
		edgeKeys:       make(map[string]struct{}),
	}
}

// addTrace processes one trace and updates accumulators (not thread-safe).
// sections gates the optional insight/template work; span counters are always on.
func (a *insightAccumulator) addTrace(t *tpackmodel.Trace, sections statsSections) {
	children := make(map[string][]string)
	traceComplete := true
	spanCount := 0
	orphanCount := 0

	for _, s := range t.Spans {
		spanCount++
		if s.ParentSpanID != "" {
			if _, ok := t.Spans[s.ParentSpanID]; !ok {
				orphanCount++
				traceComplete = false
			} else if sections.insights {
				children[s.ParentSpanID] = append(children[s.ParentSpanID], s.Feature.String())
			}
		}
	}

	a.totalSpans += spanCount
	a.orphanSpans += orphanCount
	a.totalTraces++
	if traceComplete {
		a.completeTraces++
	}

	if sections.insights {
		for spanID, s := range t.Spans {
			childList := children[spanID]
			feat := s.Feature.String()
			pc := a.parentCounters[feat]
			if pc == nil {
				pc = &parentSetCounter{
					dedupFreq:  make(map[string]int),
					countsFreq: make(map[string]int),
				}
				a.parentCounters[feat] = pc
			}
			pc.total++
			if len(childList) == 0 {
				pc.leafExecs++
			} else {
				pc.dedupFreq[strings.Join(sortedUnique(childList), ",")]++
				pc.countsFreq[strings.Join(sortedCopy(childList), ",")]++
			}
		}
	}

	if sections.templates {
		// Pre-build children-by-start-time map once for the whole trace so the
		// root loop below doesn't redo it per subtree.
		childrenOf := make(map[string][]*tpackmodel.Span)
		for _, s := range t.Spans {
			if s.ParentSpanID == "" {
				continue
			}
			if _, ok := t.Spans[s.ParentSpanID]; !ok {
				continue
			}
			childrenOf[s.ParentSpanID] = append(childrenOf[s.ParentSpanID], s)
		}
		for _, cs := range childrenOf {
			sort.Slice(cs, func(i, j int) bool { return cs[i].StartTime < cs[j].StartTime })
		}
		for spanID, s := range t.Spans {
			_, parentExists := t.Spans[s.ParentSpanID]
			if s.ParentSpanID != "" && parentExists {
				continue
			}
			hash, size := templateHashFromRoot(spanID, t.Spans, childrenOf)
			a.templateHashes[hash]++
			if _, exists := a.templateSizes[hash]; !exists {
				a.templateSizes[hash] = size
			}
		}
		// Accumulate distinct edge keys: (parent_feature, child_pos, child_feature).
		// Matches the edge probability model's edgeKey (pkg/tpackmodel/topology_model.go).
		for parentID, kids := range childrenOf {
			parent := t.Spans[parentID]
			parentFeat := parent.Feature.String()
			for pos, child := range kids {
				ek := parentFeat + "|" + strconv.Itoa(pos) + "|" + child.Feature.String()
				a.edgeKeys[ek] = struct{}{}
			}
		}
	}
}

// merge folds another accumulator into a. O(unique features + unique templates).
func (a *insightAccumulator) merge(b *insightAccumulator) {
	a.totalSpans += b.totalSpans
	a.orphanSpans += b.orphanSpans
	a.totalTraces += b.totalTraces
	a.completeTraces += b.completeTraces
	for feat, pc := range b.parentCounters {
		dst := a.parentCounters[feat]
		if dst == nil {
			a.parentCounters[feat] = pc
			continue
		}
		dst.total += pc.total
		dst.leafExecs += pc.leafExecs
		for k, v := range pc.dedupFreq {
			dst.dedupFreq[k] += v
		}
		for k, v := range pc.countsFreq {
			dst.countsFreq[k] += v
		}
	}
	for h, c := range b.templateHashes {
		a.templateHashes[h] += c
		a.templateSizes[h] = b.templateSizes[h]
	}
	for ek := range b.edgeKeys {
		a.edgeKeys[ek] = struct{}{}
	}
}

// templateHashFromRoot computes a canonical BFS-ordered FNV-64 hash of a trace
// subtree. Streaming hash avoids building a multi-megabyte intermediate string
// for large templates. childrenOf may be supplied by the caller (pre-sorted) to
// share work with insight computation; pass nil to compute it locally.
func templateHashFromRoot(rootSpanID string, spans map[string]*tpackmodel.Span, childrenOf map[string][]*tpackmodel.Span) (uint64, int) {
	if childrenOf == nil {
		childrenOf = make(map[string][]*tpackmodel.Span)
		for _, s := range spans {
			if s.ParentSpanID == "" {
				continue
			}
			if _, ok := spans[s.ParentSpanID]; !ok {
				continue
			}
			childrenOf[s.ParentSpanID] = append(childrenOf[s.ParentSpanID], s)
		}
		for _, cs := range childrenOf {
			sort.Slice(cs, func(i, j int) bool { return cs[i].StartTime < cs[j].StartTime })
		}
	}

	type entry struct {
		spanID    string
		parentIdx int32
	}
	queue := []entry{{rootSpanID, -1}}
	h := fnv.New64a()
	buf := make([]byte, 0, 128)
	idx := int32(0)
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		span := spans[e.spanID]
		ch := childrenOf[e.spanID]
		buf = buf[:0]
		if idx > 0 {
			buf = append(buf, '|')
		}
		buf = append(buf, span.Feature...)
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, int64(len(ch)), 10)
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, int64(e.parentIdx), 10)
		h.Write(buf)
		for _, c := range ch {
			queue = append(queue, entry{c.SpanID, idx})
		}
		idx++
	}
	return h.Sum64(), int(idx)
}

// finalize computes final percentages and writes them to the statsResult.
// Denominator for leaf/relay/multi: unique parent feature types (like Huye et al.).
// Top-set >50%: only for variable relays (parents with 2+ children in some executions).
func (a *insightAccumulator) finalize(result *statsResult) {
	if a.totalSpans > 0 {
		result.OrphanSpanPct = float64(a.orphanSpans) / float64(a.totalSpans) * 100
	}
	if a.totalTraces > 0 {
		result.CompleteTracePct = float64(a.completeTraces) / float64(a.totalTraces) * 100
	}

	// Classify each unique parent feature type (aligned with Huye et al.):
	//   Leaf:           max children across all executions == 0
	//   Single relay:   max children == 1
	//   Variable relay: max children > 1
	leafTypes := 0
	singleRelayTypes := 0
	variableRelayTypes := 0

	dedupAbove50 := 0
	countsAbove50 := 0

	totalParentTypes := len(a.parentCounters)

	for _, pc := range a.parentCounters {
		// Determine max children count from countsFreq entries
		maxChildren := 0
		for countsSet := range pc.countsFreq {
			// Count commas + 1 = number of children in this execution
			n := 1
			for _, c := range countsSet {
				if c == ',' {
					n++
				}
			}
			if n > maxChildren {
				maxChildren = n
			}
		}
		// If no non-leaf executions, maxChildren stays 0
		if maxChildren == 0 {
			leafTypes++
			continue
		}
		if maxChildren == 1 {
			singleRelayTypes++
			continue
		}

		// Variable relay: max children > 1
		variableRelayTypes++

		// Top-set concentration (only for variable relays)
		nonLeafExecs := pc.total - pc.leafExecs
		if nonLeafExecs < 2 {
			continue
		}

		maxDedup := 0
		for _, c := range pc.dedupFreq {
			if c > maxDedup {
				maxDedup = c
			}
		}
		maxCounts := 0
		for _, c := range pc.countsFreq {
			if c > maxCounts {
				maxCounts = c
			}
		}
		if float64(maxDedup)/float64(nonLeafExecs) > 0.5 {
			dedupAbove50++
		}
		if float64(maxCounts)/float64(nonLeafExecs) > 0.5 {
			countsAbove50++
		}
	}

	result.TotalParentTypes = totalParentTypes
	result.LeafCount = leafTypes
	result.SingleRelayCount = singleRelayTypes
	result.VariableRelayCount = variableRelayTypes
	if totalParentTypes > 0 {
		result.LeafPct = float64(leafTypes) / float64(totalParentTypes) * 100
		result.SingleRelayPct = float64(singleRelayTypes) / float64(totalParentTypes) * 100
		result.MultiChildPct = float64(variableRelayTypes) / float64(totalParentTypes) * 100
	}
	result.TopSetDedupCount = dedupAbove50
	result.TopSetCountsCount = countsAbove50
	if variableRelayTypes > 0 {
		result.TopSetDedupPct = float64(dedupAbove50) / float64(variableRelayTypes) * 100
		result.TopSetCountsPct = float64(countsAbove50) / float64(variableRelayTypes) * 100
	}

	// Template stats
	result.UniqueTemplates = len(a.templateHashes)
	result.UniqueEdges = len(a.edgeKeys)
	totalTemplateTraces := 0
	sizeHist := make(map[int]int)
	counts := make([]int, 0, len(a.templateHashes))
	for hash, count := range a.templateHashes {
		totalTemplateTraces += count
		size := a.templateSizes[hash]
		sizeHist[size]++
		counts = append(counts, count)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(counts)))
	result.TemplateTraces = totalTemplateTraces
	result.TemplateSizeHist = sizeHist
	result.TemplateCountsDesc = counts
}

// computeInsights is a convenience wrapper for batch processing (serial).
// Only used by the RE2 path. The pdata path uses the parallel worker pool instead.
func computeInsights(result *statsResult, traces []*tpackmodel.Trace) {
	acc := newInsightAccumulator()
	sec := statsSections{basic: true, insights: true, templates: true}
	for _, t := range traces {
		acc.addTrace(t, sec)
	}
	acc.finalize(result)
}

func sortedUnique(s []string) []string {
	m := make(map[string]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

// attrAccum tracks per-attribute cardinality and count for aggregation.
type attrAccum struct {
	count  int
	values map[string]struct{}
}

func addAttrVal(stats map[string]*attrAccum, key, val string) {
	a, ok := stats[key]
	if !ok {
		a = &attrAccum{values: make(map[string]struct{})}
		stats[key] = a
	}
	a.count++
	if val != "" {
		a.values[val] = struct{}{}
	}
}

// statsSections is a bitmap of which sections to compute.
type statsSections struct {
	basic     bool
	columns   bool
	insights  bool
	templates bool
}

func parseStatsSections(s string) statsSections {
	sec := statsSections{}
	for p := range strings.SplitSeq(s, ",") {
		switch strings.TrimSpace(p) {
		case "basic":
			sec.basic = true
		case "columns":
			sec.columns = true
		case "insights":
			sec.insights = true
		case "templates":
			sec.templates = true
		case "all", "":
			return statsSections{basic: true, columns: true, insights: true, templates: true}
		}
	}
	// basic is always on — it's just the span/trace counters everyone needs for context
	sec.basic = true
	return sec
}

// runDatasetStats detects the dataset format, collects statistics, and outputs results.
func runDatasetStats(inputPath string, workers int, outputJSON string, primaryAttributes []string, sectionsFlag string) error {
	sections := parseStatsSections(sectionsFlag)
	log.Printf("dataset-stats: analyzing %s (features: %v, sections: %+v)", inputPath, primaryAttributes, sections)

	reader, files, format, err := detectInputFormat(inputPath)
	if err != nil {
		return err
	}

	var result *statsResult

	if format == "re2" {
		// RE2 uses subdirectories — keep existing handler
		result, err = statsRE2Dir(inputPath, workers, primaryAttributes)
	} else {
		// Unified path: use reader + files for all pdata-based formats
		result, err = statsFromReader(reader, files, format, inputPath, primaryAttributes, sections)
	}
	if err != nil {
		return err
	}

	// Print text output
	fmt.Printf("Format:            %s\n", result.Format)
	fmt.Printf("Spans:             %d\n", result.Spans)
	fmt.Printf("Traces:            %d\n", result.Traces)
	fmt.Printf("Services:          %d\n", result.Services)
	fmt.Printf("Total bytes:       %d (%.1f MB)\n", result.TotalBytes, float64(result.TotalBytes)/(1024*1024))
	fmt.Printf("Time window:       %.0fs (%.1f min)\n", result.TimeWindowSeconds, result.TimeWindowSeconds/60)
	if result.TimeWindowSeconds > 0 {
		dailyTB := float64(result.TotalBytes) / result.TimeWindowSeconds * 86400 / (1024 * 1024 * 1024 * 1024)
		monthlyCost := dailyTB * 1024 * 30 * 0.10
		fmt.Printf("Daily volume:      %.4f TB/day\n", dailyTB)
		fmt.Printf("Monthly cost:      $%.0f/month (at $0.10/GB)\n", monthlyCost)
	}

	if sections.insights {
		fmt.Printf("\nInsight 1: Topology Repetitiveness (%d unique parent types)\n", result.TotalParentTypes)
		fmt.Printf("  Leaf types:                      %.1f%% (%d)\n", result.LeafPct, result.LeafCount)
		fmt.Printf("  Single-relay types:              %.1f%% (%d)\n", result.SingleRelayPct, result.SingleRelayCount)
		fmt.Printf("  Variable-relay types:            %.1f%% (%d)\n", result.MultiChildPct, result.VariableRelayCount)
		fmt.Printf("  Top set >50%% (unique svc):       %.1f%% (%d/%d)\n", result.TopSetDedupPct, result.TopSetDedupCount, result.VariableRelayCount)
		fmt.Printf("  Top set >50%% (w/ repetition):    %.1f%% (%d/%d)\n", result.TopSetCountsPct, result.TopSetCountsCount, result.VariableRelayCount)
		fmt.Printf("\nInsight 2: Span Loss\n")
		fmt.Printf("  Orphan spans:                    %.1f%%\n", result.OrphanSpanPct)
		fmt.Printf("  Complete traces:                 %.1f%%\n", result.CompleteTracePct)
	}

	if sections.templates {
		fmt.Printf("\nTrace Templates\n")
		fmt.Printf("  Unique templates:                %d\n", result.UniqueTemplates)
		fmt.Printf("  Total traces (subtrees):         %d\n", result.TemplateTraces)
		if len(result.TemplateSizeHist) > 0 {
			var sizes []int
			for s := range result.TemplateSizeHist {
				sizes = append(sizes, s)
			}
			sort.Ints(sizes)
			fmt.Printf("  Size distribution (nodes → unique templates):\n")
			for _, s := range sizes {
				fmt.Printf("    %3d nodes: %d\n", s, result.TemplateSizeHist[s])
			}
		}
	}

	if sections.columns && len(result.Attributes) > 0 {
		fmt.Printf("\nColumn Statistics (%d keys)\n", len(result.Attributes))
		fmt.Printf("  %-40s %12s %10s\n", "Column", "Cardinality", "Coverage")
		for _, a := range result.Attributes {
			fmt.Printf("  %-40s %12d %9.1f%%\n", a.Name, a.Cardinality, a.Coverage)
		}
	}

	// Write JSON if requested
	if outputJSON != "" {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(outputJSON, data, 0644); err != nil {
			return err
		}
		fmt.Printf("Written to:        %s\n", outputJSON)
	}

	return nil
}

func detectJSONFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if _, ok := raw["data"]; ok {
		return "jaeger", nil
	}
	if _, ok := raw["resourceSpans"]; ok {
		return "otlp", nil
	}
	return "unknown", nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// statsRE2Dir processes an RE2 parent directory with scenario subdirectories.
func statsRE2Dir(dir string, workers int, primaryAttributes []string) (*statsResult, error) {
	scenarios, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	type job struct {
		csvPath string
	}

	var jobs []job
	for _, scenario := range scenarios {
		if !scenario.IsDir() {
			continue
		}
		for _, run := range []string{"1", "2", "3"} {
			csvPath := filepath.Join(dir, scenario.Name(), run, "traces.csv")
			if _, err := os.Stat(csvPath); err == nil {
				jobs = append(jobs, job{csvPath})
			}
		}
	}

	if len(jobs) == 0 {
		return nil, fmt.Errorf("no traces.csv files found in %s", dir)
	}

	type runResult struct {
		spans    int
		traces   int
		services map[string]struct{}
		bytes    int64
		minTime  int64
		maxTime  int64
		traceMap map[string]*tpackmodel.Trace
	}

	results := make([]runResult, len(jobs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, j job) {
			defer wg.Done()
			defer func() { <-sem }()

			spans, traces, svcs, bytes, minT, maxT, tm := countCSVFileExtended(j.csvPath, primaryAttributes)
			results[idx] = runResult{spans, traces, svcs, bytes, minT, maxT, tm}
		}(i, j)
	}
	wg.Wait()

	totalSpans := 0
	totalTraces := 0
	var totalBytes int64
	allServices := make(map[string]struct{})
	var allTraces []*tpackmodel.Trace

	for _, r := range results {
		totalSpans += r.spans
		totalTraces += r.traces
		totalBytes += r.bytes
		for s := range r.services {
			allServices[s] = struct{}{}
		}
		for _, t := range r.traceMap {
			allTraces = append(allTraces, t)
		}
	}

	nRuns := max(len(results), 1)
	var totalWindow float64
	for _, r := range results {
		if r.maxTime > r.minTime {
			totalWindow += float64(r.maxTime-r.minTime) / 1e6
		}
	}
	avgWindowSeconds := totalWindow / float64(nRuns)

	result := &statsResult{
		Format:            fmt.Sprintf("RE2 directory (%d runs)", len(results)),
		Spans:             totalSpans / nRuns,
		Traces:            totalTraces / nRuns,
		Services:          len(allServices),
		TotalBytes:        totalBytes / int64(nRuns),
		TimeWindowSeconds: avgWindowSeconds,
	}
	computeInsights(result, allTraces)
	return result, nil
}

// csvColMap maps feature column names to CSV column names.
var csvColMap = map[string]string{
	"service.name":     "serviceName",
	"operation.name":   "operationName",
	"status.code":      "statusCode",
	"span.kind":        "spanKind",
	"http.status_code": "httpStatusCode",
}

func countCSVFileExtended(path string, primaryAttributes []string) (spans int, traces int, services map[string]struct{}, totalBytes int64, minTime int64, maxTime int64, traceMap map[string]*tpackmodel.Trace) {
	services = make(map[string]struct{})
	traceMap = make(map[string]*tpackmodel.Trace)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	fi, _ := f.Stat()
	totalBytes = fi.Size()

	r := csv.NewReader(f)
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return
	}

	// Build column index from header
	colIdx := make(map[string]int)
	for i, col := range header {
		colIdx[strings.TrimSpace(col)] = i
	}

	traceIdx, hasTrace := colIdx["traceID"]
	if !hasTrace {
		return
	}
	spanIDIdx, hasSpanID := colIdx["spanID"]
	parentIDIdx, hasParentID := colIdx["parentSpanID"]
	svcIdx, hasSvc := colIdx["serviceName"]
	startTimeIdx, hasStartTime := colIdx["startTime"]

	// Map feature columns to CSV indices
	type featCol struct {
		name string
		idx  int
	}
	var featCols []featCol
	for _, fc := range primaryAttributes {
		csvName := fc
		if mapped, ok := csvColMap[fc]; ok {
			csvName = mapped
		}
		if idx, ok := colIdx[csvName]; ok {
			featCols = append(featCols, featCol{fc, idx})
		}
	}

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		spans++

		traceID := record[traceIdx]
		if hasSvc && svcIdx < len(record) {
			services[record[svcIdx]] = struct{}{}
		}

		// Build feature
		vals := make(map[string]string, len(featCols))
		for _, fc := range featCols {
			if fc.idx < len(record) {
				vals[fc.name] = record[fc.idx]
			}
		}
		feature := tpackmodel.NewSpanFeature(primaryAttributes, vals)

		sid := ""
		if hasSpanID {
			sid = record[spanIDIdx]
		}
		pid := ""
		if hasParentID && parentIDIdx < len(record) {
			pid = record[parentIDIdx]
		}

		td, ok := traceMap[traceID]
		if !ok {
			td = &tpackmodel.Trace{TraceID: traceID, Spans: make(map[string]*tpackmodel.Span)}
			traceMap[traceID] = td
		}
		td.Spans[sid] = &tpackmodel.Span{
			SpanID:       sid,
			ParentSpanID: pid,
			Feature:      feature,
		}

		if hasStartTime && startTimeIdx < len(record) {
			if t, err := parseInt64(record[startTimeIdx]); err == nil {
				if minTime == 0 || t < minTime {
					minTime = t
				}
				if t > maxTime {
					maxTime = t
				}
			}
		}
	}
	traces = len(traceMap)
	return
}

// statsFromReader reads files in parallel, fusing trace conversion, insight
// accumulation, and attribute stats collection into a single pass per file.
// Each worker owns an insightAccumulator + attr-stats map; results are merged
// at the end. Encoder-free template hashing makes the insight merge cheap.
func statsFromReader(reader fileReader, files []string, format, inputPath string, primaryAttributes []string, sections statsSections) (*statsResult, error) {
	nw := max(min(runtime.NumCPU(), len(files)), 1)

	type workerResult struct {
		totalSpans int
		traces     int
		services   map[string]struct{}
		minTime    int64
		maxTime    int64
		insight    *insightAccumulator
		attrStats  map[string]*attrAccum
	}

	workers := make([]workerResult, nw)
	var processed int32
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

	for w := range nw {
		workers[w] = workerResult{
			services:  make(map[string]struct{}),
			insight:   newInsightAccumulator(),
			attrStats: make(map[string]*attrAccum),
		}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ws := &workers[w]
			for i := w; i < len(files); i += nw {
				td, err := reader(files[i])
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("read %s: %w", files[i], err)
					}
					errMu.Unlock()
					return
				}

				// Walk pdata once: basic counters + optional attribute stats.
				for ri := 0; ri < td.ResourceSpans().Len(); ri++ {
					rs := td.ResourceSpans().At(ri)
					svcName := ""
					if sn, ok := rs.Resource().Attributes().Get("service.name"); ok {
						svcName = sn.AsString()
					}
					for j := 0; j < rs.ScopeSpans().Len(); j++ {
						ss := rs.ScopeSpans().At(j)
						for k := 0; k < ss.Spans().Len(); k++ {
							span := ss.Spans().At(k)
							ws.totalSpans++
							if svcName != "" {
								ws.services[svcName] = struct{}{}
							}
							if sections.columns {
								if svcName != "" {
									addAttrVal(ws.attrStats, "service.name", svcName)
								}
								if span.Name() != "" {
									addAttrVal(ws.attrStats, "operation.name", span.Name())
								}
								addAttrVal(ws.attrStats, "span.kind", span.Kind().String())
								addAttrVal(ws.attrStats, "status.code", span.Status().Code().String())
								span.Attributes().Range(func(key string, val pcommon.Value) bool {
									addAttrVal(ws.attrStats, key, val.AsString())
									return true
								})
							}
							st := int64(span.StartTimestamp()) / 1000
							if ws.minTime == 0 || st < ws.minTime {
								ws.minTime = st
							}
							if st > ws.maxTime {
								ws.maxTime = st
							}
						}
					}
				}

				// Insight/template work requires tpackmodel.Trace (per-trace children map).
				if sections.insights || sections.templates {
					traces := otlpconv.FromPdata(td, primaryAttributes, nil)
					ws.traces += len(traces)
					for _, t := range traces {
						ws.insight.addTrace(t, sections)
					}
				} else {
					// Cheap trace counter: unique trace IDs only.
					seen := make(map[[16]byte]struct{})
					for ri := 0; ri < td.ResourceSpans().Len(); ri++ {
						rs := td.ResourceSpans().At(ri)
						for j := 0; j < rs.ScopeSpans().Len(); j++ {
							ss := rs.ScopeSpans().At(j)
							for k := 0; k < ss.Spans().Len(); k++ {
								tid := ss.Spans().At(k).TraceID()
								seen[tid] = struct{}{}
							}
						}
					}
					ws.traces += len(seen)
				}

				done := atomic.AddInt32(&processed, 1)
				if done%10 == 0 || int(done) == len(files) {
					log.Printf("  processed %d/%d files", done, len(files))
				}
			}
		}(w)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	// Merge worker results.
	totalSpans := 0
	totalTraces := 0
	services := make(map[string]struct{})
	mergedAttrs := make(map[string]*attrAccum)
	mergedInsight := newInsightAccumulator()
	var minTime, maxTime int64
	for w := range workers {
		ws := &workers[w]
		totalSpans += ws.totalSpans
		totalTraces += ws.traces
		for s := range ws.services {
			services[s] = struct{}{}
		}
		if ws.minTime != 0 && (minTime == 0 || ws.minTime < minTime) {
			minTime = ws.minTime
		}
		if ws.maxTime > maxTime {
			maxTime = ws.maxTime
		}
		for k, a := range ws.attrStats {
			m, ok := mergedAttrs[k]
			if !ok {
				mergedAttrs[k] = a
				continue
			}
			m.count += a.count
			for v := range a.values {
				m.values[v] = struct{}{}
			}
		}
		mergedInsight.merge(ws.insight)
	}

	// Total bytes from input dir.
	var totalBytes int64
	info, _ := os.Stat(inputPath)
	if info != nil && info.IsDir() {
		entries, _ := os.ReadDir(inputPath)
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				totalBytes += fi.Size()
			}
		}
	} else if info != nil {
		totalBytes = info.Size()
	}

	windowSeconds := float64(maxTime-minTime) / 1e6

	result := &statsResult{
		Format:            fmt.Sprintf("%s (%d traces)", format, totalTraces),
		Spans:             totalSpans,
		Traces:            totalTraces,
		Services:          len(services),
		TotalBytes:        totalBytes,
		TimeWindowSeconds: windowSeconds,
	}
	mergedInsight.finalize(result)

	// Build attribute entries sorted by coverage.
	entries := make([]attrStatEntry, 0, len(mergedAttrs))
	for name, a := range mergedAttrs {
		cov := 0.0
		if totalSpans > 0 {
			cov = float64(a.count) / float64(totalSpans) * 100
		}
		entries = append(entries, attrStatEntry{
			Name:        name,
			Cardinality: len(a.values),
			Count:       a.count,
			Coverage:    cov,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Coverage > entries[j].Coverage
	})
	result.Attributes = entries

	return result, nil
}
