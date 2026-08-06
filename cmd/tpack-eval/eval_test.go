package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// makeTestTraces creates test data: 2 traces across 2 minute buckets.
// Trace 1 (bucket 16): frontend→backend, both status.code "0", http 200
// Trace 2 (bucket 17): frontend→payment, payment has status.code "2", http 500
var testFC = tpackmodel.DefaultFeatureColumns

func testFeat(values map[string]string) tpackmodel.SpanFeature {
	return tpackmodel.NewSpanFeature(testFC, values)
}

// makeTestEvalSpan creates an evalSpan with raw metadata (no eager merge).
// metadataGroupKeys iterates both Feature + Metadata at eval time.
func makeTestEvalSpan(spanID, parentSpanID string, feat tpackmodel.SpanFeature, meta map[string]string, startTime, duration int64) *evalSpan {
	return &evalSpan{
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		Feature:      feat,
		Metadata:     meta,
		StartTime:    startTime,
		Duration:     duration,
	}
}

func makeTestTraces() []evalTrace {
	return []evalTrace{
		{
			"span1a": makeTestEvalSpan("span1a", "",
				testFeat(map[string]string{"service.name": "frontend", "operation.name": "/api", "status.code": "0"}),
				map[string]string{"http.status_code": "200"},
				1000000000, 5000), // 1000000000 / 60000000 = 16
			"span1b": makeTestEvalSpan("span1b", "span1a",
				testFeat(map[string]string{"service.name": "backend", "operation.name": "SELECT", "status.code": "0"}),
				map[string]string{"http.status_code": "200"},
				1000001000, 3000),
		},
		{
			"span2a": makeTestEvalSpan("span2a", "",
				testFeat(map[string]string{"service.name": "frontend", "operation.name": "/checkout", "status.code": "0"}),
				nil,
				1060000000, 8000), // 1060000000 / 60000000 = 17
			"span2b": makeTestEvalSpan("span2b", "span2a",
				testFeat(map[string]string{"service.name": "payment", "operation.name": "Process", "status.code": "2"}),
				map[string]string{"http.status_code": "500"},
				1060001000, 4000),
		},
	}
}

func TestHelpers(t *testing.T) {
	// timeBucketMinute: 1000000000 μs = 1000s ≈ 16.67 min → bucket 16
	if got := timeBucketMinute(1000000000); got != 16 {
		t.Errorf("timeBucketMinute(1000000000) = %d, want 16", got)
	}
	if got := timeBucketMinute(1060000000); got != 17 {
		t.Errorf("timeBucketMinute(1060000000) = %d, want 17", got)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	tests := []struct {
		p    float64
		want float64
	}{
		{0, 1.0},
		{10, 1.9},
		{50, 5.5},
		{90, 9.1},
		{100, 10.0},
	}

	for _, tt := range tests {
		got := percentile(sorted, tt.p)
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("percentile(sorted, %.0f) = %f, want %f", tt.p, got, tt.want)
		}
	}

	// Edge cases
	if got := percentile([]float64{42}, 50); got != 42 {
		t.Errorf("percentile([42], 50) = %f, want 42", got)
	}
	if got := percentile([]float64{}, 50); got != 0 {
		t.Errorf("percentile([], 50) = %f, want 0", got)
	}
}

func TestEvaluateGraph(t *testing.T) {
	dir := t.TempDir()
	traces := makeTestTraces()

	if err := evaluateGraph(dir, traces); err != nil {
		t.Fatal(err)
	}

	var result struct {
		ServiceGraphByTime map[string]struct {
			Nodes []string       `json:"nodes"`
			Edges map[string]int `json:"edges"`
		} `json:"service_graph_by_time"`
	}
	readJSON(t, filepath.Join(dir, "graph_results.json"), &result)

	// Bucket 16: frontend//api → backend/SELECT
	b16 := result.ServiceGraphByTime["16"]
	if len(b16.Nodes) != 2 {
		t.Errorf("bucket 16 nodes = %v, want 2 nodes", b16.Nodes)
	}
	if b16.Edges["frontend//api->backend/SELECT"] != 1 {
		t.Errorf("bucket 16 edge frontend//api->backend/SELECT = %d, want 1", b16.Edges["frontend//api->backend/SELECT"])
	}

	// Bucket 17: frontend//checkout → payment/Process
	b17 := result.ServiceGraphByTime["17"]
	if len(b17.Nodes) != 2 {
		t.Errorf("bucket 17 nodes = %v, want 2 nodes", b17.Nodes)
	}
	if b17.Edges["frontend//checkout->payment/Process"] != 1 {
		t.Errorf("bucket 17 edge frontend//checkout->payment/Process = %d, want 1", b17.Edges["frontend//checkout->payment/Process"])
	}
}

func TestEvaluateRate(t *testing.T) {
	dir := t.TempDir()
	traces := makeTestTraces()

	if err := evaluateRate(dir, traces); err != nil {
		t.Fatal(err)
	}

	var result struct {
		SpanRateByTime    map[string]map[string]int `json:"span_rate_by_time"`
		TotalSpansByGroup map[string]int            `json:"total_spans_by_group"`
	}
	readJSON(t, filepath.Join(dir, "rate_over_time_results.json"), &result)

	// Check total counts
	if result.TotalSpansByGroup["all"] != 4 {
		t.Errorf("total all = %d, want 4", result.TotalSpansByGroup["all"])
	}
	if result.TotalSpansByGroup["service.name:frontend"] != 2 {
		t.Errorf("total frontend = %d, want 2", result.TotalSpansByGroup["service.name:frontend"])
	}

	// Check per-bucket: "all" in bucket 16 should be 2
	if result.SpanRateByTime["all"]["16"] != 2 {
		t.Errorf("rate all/16 = %d, want 2", result.SpanRateByTime["all"]["16"])
	}

	// http.status_code:200 should have 2 spans (both in bucket 16)
	if result.TotalSpansByGroup["http.status_code:200"] != 2 {
		t.Errorf("total http 200 = %d, want 2", result.TotalSpansByGroup["http.status_code:200"])
	}
}

func TestEvaluateError(t *testing.T) {
	dir := t.TempDir()
	traces := makeTestTraces()

	if err := evaluateError(dir, traces); err != nil {
		t.Fatal(err)
	}

	var result struct {
		SpanRateByTime    map[string]map[string]int `json:"span_rate_by_time"`
		TotalSpansByGroup map[string]int            `json:"total_spans_by_group"`
	}
	readJSON(t, filepath.Join(dir, "error_over_time_results.json"), &result)

	// Only span2b has status.code "2"
	if result.TotalSpansByGroup["all"] != 1 {
		t.Errorf("error total all = %d, want 1", result.TotalSpansByGroup["all"])
	}
	if result.TotalSpansByGroup["service.name:payment"] != 1 {
		t.Errorf("error total payment = %d, want 1", result.TotalSpansByGroup["service.name:payment"])
	}

	// Error span is in bucket 17
	if result.SpanRateByTime["all"]["17"] != 1 {
		t.Errorf("error rate all/17 = %d, want 1", result.SpanRateByTime["all"]["17"])
	}

	// No errors in bucket 16
	if result.SpanRateByTime["all"]["16"] != 0 {
		t.Errorf("error rate all/16 = %d, want 0", result.SpanRateByTime["all"]["16"])
	}
}

func TestEvaluateDuration(t *testing.T) {
	dir := t.TempDir()
	traces := makeTestTraces()

	if err := evaluateDuration(dir, traces); err != nil {
		t.Fatal(err)
	}

	var result struct {
		DurationPercentilesByTime map[string]map[string]map[string]float64 `json:"duration_percentiles_by_time"`
		TotalSpansByGroup         map[string]int                           `json:"total_spans_by_group"`
	}
	readJSON(t, filepath.Join(dir, "duration_over_time_results.json"), &result)

	// Bucket 16: durations [5000, 3000] → sorted [3000, 5000]
	// percentile(sorted, p) with linear interpolation on n=2: idx = p/100 * 1
	// p50: idx=0.5 → 3000 + 0.5*2000 = 4000
	// p90: idx=0.9 → 3000 + 0.9*2000 = 4800
	// p99: idx=0.99 → 3000 + 0.99*2000 = 4980
	allB16 := result.DurationPercentilesByTime["all"]["16"]
	if allB16["p50"] != 4000 {
		t.Errorf("all/16/p50 = %f, want 4000", allB16["p50"])
	}
	if allB16["p90"] != 4800 {
		t.Errorf("all/16/p90 = %f, want 4800", allB16["p90"])
	}
	if allB16["p99"] != 4980 {
		t.Errorf("all/16/p99 = %f, want 4980", allB16["p99"])
	}

	// Total spans
	if result.TotalSpansByGroup["all"] != 4 {
		t.Errorf("total all = %d, want 4", result.TotalSpansByGroup["all"])
	}
}

func TestEvaluateSpanCount(t *testing.T) {
	dir := t.TempDir()
	traces := makeTestTraces()

	if err := evaluateSpanCount(dir, traces); err != nil {
		t.Fatal(err)
	}

	var result struct {
		SpanCount map[string][]int `json:"span_count"`
	}
	readJSON(t, filepath.Join(dir, "span_count_results.json"), &result)

	sizes := result.SpanCount["all"]
	sort.Ints(sizes)
	// Both traces have 2 spans in a single tree
	if len(sizes) != 2 || sizes[0] != 2 || sizes[1] != 2 {
		t.Errorf("span_count/all = %v, want [2, 2]", sizes)
	}
}

func TestEvaluateSpanCountMultipleTrees(t *testing.T) {
	dir := t.TempDir()
	// Trace with 3 spans: 2 connected + 1 orphan = trees of size [2, 1]
	svcFeat := testFeat(map[string]string{"service.name": "svc", "operation.name": "o", "status.code": "0"})
	traces := []evalTrace{
		{
			"a": &evalSpan{SpanID: "a", ParentSpanID: "", Feature: svcFeat, StartTime: 0, Duration: 100},
			"b": &evalSpan{SpanID: "b", ParentSpanID: "a", Feature: svcFeat, StartTime: 0, Duration: 50},
			"c": &evalSpan{SpanID: "c", ParentSpanID: "missing", Feature: svcFeat, StartTime: 0, Duration: 30},
		},
	}

	if err := evaluateSpanCount(dir, traces); err != nil {
		t.Fatal(err)
	}

	var result struct {
		SpanCount map[string][]int `json:"span_count"`
	}
	readJSON(t, filepath.Join(dir, "span_count_results.json"), &result)

	sizes := result.SpanCount["all"]
	sort.Ints(sizes)
	if len(sizes) != 2 || sizes[0] != 1 || sizes[1] != 2 {
		t.Errorf("span_count/all = %v, want [1, 2]", sizes)
	}
}

func TestEvaluateTime(t *testing.T) {
	dir := t.TempDir()

	if err := evaluateTime(dir, 1.5, 0.3); err != nil {
		t.Fatal(err)
	}

	var result map[string]float64
	readJSON(t, filepath.Join(dir, "time_results.json"), &result)

	if result["total_time_seconds"] != 1.5 {
		t.Errorf("total = %f, want 1.5", result["total_time_seconds"])
	}
	if result["compression_time_seconds"] != 1.2 {
		t.Errorf("compress = %f, want 1.2", result["compression_time_seconds"])
	}
	if result["decompression_time_seconds"] != 0.3 {
		t.Errorf("decompress = %f, want 0.3", result["decompression_time_seconds"])
	}
}

func TestRunAllEvaluators(t *testing.T) {
	dir := t.TempDir()
	traces := makeTestTraces()

	if err := runAllEvaluators(dir, traces, nil, 2.5, 0.5); err != nil {
		t.Fatal(err)
	}

	// Verify evaluation files exist
	expectedFiles := []string{
		"graph_results.json",
		"rate_over_time_results.json",
		"error_over_time_results.json",
		"duration_over_time_results.json",
		"span_count_results.json",
		"time_results.json",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(dir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("missing file: %s", f)
		}
	}
}

// readJSON reads and unmarshals a JSON file into v.
func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v\ndata: %s", path, err, data)
	}
}
