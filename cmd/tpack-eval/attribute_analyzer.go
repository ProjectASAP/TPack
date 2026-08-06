package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// pairKey identifies a (parent feature, child feature) pair.
type pairKey struct {
	ParentFeature string
	ChildFeature  string
}

// pairSample holds duration ratios grouped by metadata value for one (parent, child) pair.
type pairSample struct {
	Groups map[string][]float64 // metaValue → durRatios
	Total  int
}

// etaResult holds the η² result for one (parent, child) pair and one metadata column.
type etaResult struct {
	Pair   pairKey
	Eta2   float64
	N      int
	Groups map[string]int // metaValue → count
}

// columnResult holds aggregated η² results for one metadata column.
type columnResult struct {
	Column     string
	WeightedH2 float64
	PairCount  int
	MaxEta2    float64
	MaxPair    pairKey
	Details    []etaResult
}

func runAnalyzeAttributes(inputPath string, bucketDurationUs int64, primaryAttributes []string, dependentAttributes []string) error {
	if len(dependentAttributes) == 0 {
		return fmt.Errorf("--dependent-attributes is required for --analyze-attributes")
	}

	t0 := time.Now()
	log.Printf("Reading %s ...", inputPath)
	buckets, err := readOTLP(inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	totalTraces := 0
	totalSpans := 0
	for _, traces := range buckets {
		totalTraces += len(traces)
		for _, t := range traces {
			totalSpans += len(t.Spans)
		}
	}
	log.Printf("Read %d traces (%d spans) in %d buckets [%.1fs]",
		totalTraces, totalSpans, len(buckets), time.Since(t0).Seconds())

	// Fit node encoder across all traces
	var allTraces []*tpackmodel.Trace
	for _, traces := range buckets {
		allTraces = append(allTraces, traces...)
	}
	encoder := tpackmodel.NewNodeEncoder()
	encoder.Fit(tpackmodel.CollectFeatures(allTraces))

	// For each metadata column, collect (parent, child) → grouped durRatios
	results := make([]columnResult, len(dependentAttributes))

	for colIdx, col := range dependentAttributes {
		pairs := make(map[pairKey]*pairSample)

		for _, t := range allTraces {
			// Build parent→children
			parentChildren := make(map[string][]*tpackmodel.Span)
			for _, s := range t.Spans {
				if s.ParentSpanID != "" {
					if _, ok := t.Spans[s.ParentSpanID]; ok {
						parentChildren[s.ParentSpanID] = append(parentChildren[s.ParentSpanID], s)
					}
				}
			}

			for parentID, children := range parentChildren {
				parent := t.Spans[parentID]
				if parent.Duration <= 0 {
					continue
				}

				parentFeature := parent.Feature.String()

				for _, child := range children {
					childFeature := child.Feature.String()
					durRatio := clamp(float64(child.Duration)/float64(parent.Duration), 0, 1)

					metaVal := ""
					if child.Metadata != nil {
						metaVal = child.Metadata[col]
					}

					pk := pairKey{ParentFeature: parentFeature, ChildFeature: childFeature}
					ps, ok := pairs[pk]
					if !ok {
						ps = &pairSample{Groups: make(map[string][]float64)}
						pairs[pk] = ps
					}
					ps.Groups[metaVal] = append(ps.Groups[metaVal], durRatio)
					ps.Total++
				}
			}
		}

		// Compute η² for each pair
		var details []etaResult
		totalWeightedEta := 0.0
		totalWeight := 0

		for pk, ps := range pairs {
			// Need at least 2 groups with ≥5 samples each
			var validGroups [][]float64
			groupCounts := make(map[string]int)
			for val, ratios := range ps.Groups {
				if len(ratios) >= 5 {
					validGroups = append(validGroups, ratios)
					groupCounts[val] = len(ratios)
				}
			}
			if len(validGroups) < 2 {
				continue
			}

			H, n := kruskalWallisH(validGroups)
			eta2 := H / float64(n-1)
			if eta2 < 0 {
				eta2 = 0
			}

			details = append(details, etaResult{
				Pair:   pk,
				Eta2:   eta2,
				N:      n,
				Groups: groupCounts,
			})
			totalWeightedEta += float64(n) * eta2
			totalWeight += n
		}

		// Sort details by η² descending
		sort.Slice(details, func(i, j int) bool {
			return details[i].Eta2 > details[j].Eta2
		})

		cr := columnResult{
			Column:    col,
			PairCount: len(details),
			Details:   details,
		}
		if totalWeight > 0 {
			cr.WeightedH2 = totalWeightedEta / float64(totalWeight)
		}
		if len(details) > 0 {
			cr.MaxEta2 = details[0].Eta2
			cr.MaxPair = details[0].Pair
		}
		results[colIdx] = cr
	}

	// Print results
	fmt.Printf("\n=== Metadata η² Analysis ===\n")
	fmt.Printf("Input: %s (%d buckets, %d traces, %d spans)\n",
		inputPath, len(buckets), totalTraces, totalSpans)
	if len(primaryAttributes) == 0 {
		fmt.Printf("Feature columns: (none)\n")
	} else {
		fmt.Printf("Feature columns: %v\n", primaryAttributes)
	}
	fmt.Printf("Metadata columns: %v\n\n", dependentAttributes)

	fmt.Printf("%-30s %8s %6s %8s   %s\n", "Column", "Wtd η²", "Pairs", "Max η²", "Max η² Pair")
	for _, cr := range results {
		maxPairLabel := featurePairLabel(cr.MaxPair)
		fmt.Printf("%-30s %8.4f %6d %8.4f   %s\n",
			cr.Column, cr.WeightedH2, cr.PairCount, cr.MaxEta2, maxPairLabel)
	}

	// Recommend columns to promote
	fmt.Printf("\nPromote to feature (η² > 0.06):\n")
	any := false
	for _, cr := range results {
		if cr.WeightedH2 > 0.06 {
			fmt.Printf("  %s\n", cr.Column)
			any = true
		}
	}
	if !any {
		fmt.Printf("  (none)\n")
	}

	// Print per-pair details for high-η² columns
	for _, cr := range results {
		if cr.WeightedH2 <= 0.06 || len(cr.Details) == 0 {
			continue
		}
		fmt.Printf("\nPer-pair detail for %s:\n", cr.Column)
		limit := min(len(cr.Details), 20)
		for _, d := range cr.Details[:limit] {
			label := featurePairLabel(d.Pair)
			groupStr := formatGroups(d.Groups)
			fmt.Printf("  %s:  η²=%.4f  n=%d  groups: %s\n", label, d.Eta2, d.N, groupStr)
		}
		if len(cr.Details) > limit {
			fmt.Printf("  ... and %d more pairs\n", len(cr.Details)-limit)
		}
	}

	return nil
}

// kruskalWallisH computes the Kruskal-Wallis H statistic.
// groups is a slice of value slices, one per group.
// Returns H and the total sample count n.
func kruskalWallisH(groups [][]float64) (float64, int) {
	// Pool all values with group labels
	type ranked struct {
		value    float64
		groupIdx int
	}
	n := 0
	for _, g := range groups {
		n += len(g)
	}
	if n < 2 {
		return 0, n
	}

	pool := make([]ranked, 0, n)
	for gi, g := range groups {
		for _, v := range g {
			pool = append(pool, ranked{value: v, groupIdx: gi})
		}
	}

	// Sort by value
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].value < pool[j].value
	})

	// Assign average ranks for ties
	ranks := make([]float64, n)
	i := 0
	for i < n {
		j := i
		for j < n && pool[j].value == pool[i].value {
			j++
		}
		avgRank := float64(i+j+1) / 2.0 // ranks are 1-based
		for k := i; k < j; k++ {
			ranks[k] = avgRank
		}
		i = j
	}

	// Compute per-group rank sums and sizes
	groupN := make([]int, len(groups))
	groupRankSum := make([]float64, len(groups))
	for idx, r := range pool {
		groupN[r.groupIdx]++
		groupRankSum[r.groupIdx] += ranks[idx]
	}

	// H = (12 / (n(n+1))) × Σ(n_j × (R̄_j - (n+1)/2)²)
	nf := float64(n)
	grandMeanRank := (nf + 1) / 2.0
	var ssb float64
	for gi := range groups {
		if groupN[gi] == 0 {
			continue
		}
		meanRank := groupRankSum[gi] / float64(groupN[gi])
		diff := meanRank - grandMeanRank
		ssb += float64(groupN[gi]) * diff * diff
	}

	H := (12.0 / (nf * (nf + 1))) * ssb

	// Tie correction
	tieCorrection := 1.0
	var tieSum float64
	ti := 0
	for ti < n {
		tj := ti
		for tj < n && pool[tj].value == pool[ti].value {
			tj++
		}
		t := float64(tj - ti)
		if t > 1 {
			tieSum += t*t*t - t
		}
		ti = tj
	}
	if tieSum > 0 {
		tieCorrection = 1.0 - tieSum/(nf*nf*nf-nf)
	}
	if tieCorrection > 0 {
		H /= tieCorrection
	}

	return H, n
}

// featurePairLabel formats a pairKey for human-readable display.
func featurePairLabel(pk pairKey) string {
	if pk.ParentFeature == "" && pk.ChildFeature == "" {
		return "(all spans)"
	}

	pf := tpackmodel.ParseSpanFeature(pk.ParentFeature)
	cf := tpackmodel.ParseSpanFeature(pk.ChildFeature)

	parentLabel := pf.ServiceName()
	if pf.SpanKind() != "" {
		parentLabel += "(" + pf.SpanKind() + ")"
	}
	childLabel := cf.ServiceName()
	if cf.OperationName() != "" {
		childLabel += "(" + cf.OperationName() + ")"
	} else if cf.SpanKind() != "" {
		childLabel += "(" + cf.SpanKind() + ")"
	}

	return parentLabel + " → " + childLabel
}

// formatGroups formats metadata group counts for display.
func formatGroups(groups map[string]int) string {
	type kv struct {
		k string
		v int
	}
	sorted := make([]kv, 0, len(groups))
	for k, v := range groups {
		label := k
		if label == "" {
			label = "<empty>"
		}
		sorted = append(sorted, kv{label, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].v > sorted[j].v
	})

	var s strings.Builder
	s.WriteString("[")
	for i, kv := range sorted {
		if i > 0 {
			s.WriteString(", ")
		}
		s.WriteString(fmt.Sprintf("%s: %d", kv.k, kv.v))
	}
	s.WriteString("]")
	return s.String()
}

// etaSquared computes η² = H / (n - 1) for the given groups, applying
// the minimum sample and group count filters. Returns 0 if filters not met.
func etaSquared(groups [][]float64) float64 {
	var validGroups [][]float64
	for _, g := range groups {
		if len(g) >= 5 {
			validGroups = append(validGroups, g)
		}
	}
	if len(validGroups) < 2 {
		return 0
	}
	H, n := kruskalWallisH(validGroups)
	if n <= 1 {
		return 0
	}
	eta2 := H / float64(n-1)
	return math.Max(eta2, 0)
}

// wellKnownColumns are columns derived from resource/span properties (not span attributes).
// They always have 100% coverage when present.
var wellKnownColumns = []string{"service.name", "span.kind", "operation.name", "status.code"}

// discoverDependentAttributes scans the input data and returns all attribute names
// (plus well-known columns) that appear on at least minCoverage fraction of spans.
// Columns already in primaryAttributes are excluded.
func discoverDependentAttributes(inputPath string, primaryAttributes []string, minCoverage float64) ([]string, error) {
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", inputPath)
	}

	files, err := filepath.Glob(filepath.Join(inputPath, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .json files in %s", inputPath)
	}
	sort.Strings(files)

	attrCounts := make(map[string]int)
	totalSpans := 0

	for _, file := range files {
		td, err := readOTLPFile(file)
		if err != nil {
			return nil, err
		}

		for i := 0; i < td.ResourceSpans().Len(); i++ {
			rs := td.ResourceSpans().At(i)
			for j := 0; j < rs.ScopeSpans().Len(); j++ {
				ss := rs.ScopeSpans().At(j)
				for k := 0; k < ss.Spans().Len(); k++ {
					span := ss.Spans().At(k)
					totalSpans++
					span.Attributes().Range(func(key string, _ pcommon.Value) bool {
						attrCounts[key]++
						return true
					})
				}
			}
		}
	}

	if totalSpans == 0 {
		return nil, fmt.Errorf("no spans found")
	}

	// Well-known columns always have 100% coverage
	for _, col := range wellKnownColumns {
		attrCounts[col] = totalSpans
	}

	featureSet := make(map[string]bool, len(primaryAttributes))
	for _, col := range primaryAttributes {
		featureSet[col] = true
	}

	threshold := int(float64(totalSpans) * minCoverage)
	var columns []string
	for attr, count := range attrCounts {
		if count >= threshold && !featureSet[attr] {
			columns = append(columns, attr)
		}
	}

	// Sort by coverage descending for deterministic, readable output
	sort.Slice(columns, func(i, j int) bool {
		return attrCounts[columns[i]] > attrCounts[columns[j]]
	})

	log.Printf("Attribute discovery: %d total spans, %d attributes, %d above %.0f%% coverage",
		totalSpans, len(attrCounts), len(columns), minCoverage*100)

	return columns, nil
}
