package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// makeTestInputOTLP creates a synthetic OTLP JSON file with numTraces traces (2 spans each).
func makeTestInputOTLP(t *testing.T, dir string, numTraces int) string {
	t.Helper()
	path := filepath.Join(dir, "traces.json")

	td := ptrace.NewTraces()

	// Create traces grouped by service
	rsFrontend := td.ResourceSpans().AppendEmpty()
	rsFrontend.Resource().Attributes().PutStr("service.name", "frontend")
	ssFrontend := rsFrontend.ScopeSpans().AppendEmpty()

	rsBackend := td.ResourceSpans().AppendEmpty()
	rsBackend.Resource().Attributes().PutStr("service.name", "backend")
	ssBackend := rsBackend.ScopeSpans().AppendEmpty()

	for i := range numTraces {
		traceID := "00000000000000000000000000" + leftPad(strconv.Itoa(i), 6, '0')
		rootSpanID := "000000000000" + leftPad(strconv.Itoa(i*2), 4, '0')
		childSpanID := "000000000000" + leftPad(strconv.Itoa(i*2+1), 4, '0')
		startTimeUs := int64(1000000000) + int64(i)*30000000
		startTimeNs := startTimeUs * 1000

		// Root span (frontend)
		rootSpan := ssFrontend.Spans().AppendEmpty()
		rootSpan.SetTraceID(hexToTraceID(traceID))
		rootSpan.SetSpanID(hexToSpanID(rootSpanID))
		rootSpan.SetName("/api/test")
		rootSpan.Attributes().PutStr("http.status_code", "200")
		rootSpan.Attributes().PutStr("http.method", "GET")
		rootSpan.Status().SetCode(ptrace.StatusCodeUnset)
		rootSpan.SetStartTimestamp(pcommon.Timestamp(startTimeNs))
		rootSpan.SetEndTimestamp(pcommon.Timestamp(startTimeNs + 5000000)) // +5000 μs

		// Child span (backend)
		childSpan := ssBackend.Spans().AppendEmpty()
		childSpan.SetTraceID(hexToTraceID(traceID))
		childSpan.SetSpanID(hexToSpanID(childSpanID))
		childSpan.SetParentSpanID(hexToSpanID(rootSpanID))
		childSpan.SetName("SELECT")
		childSpan.Attributes().PutStr("http.status_code", "200")
		childSpan.Status().SetCode(ptrace.StatusCodeUnset)
		childSpan.SetStartTimestamp(pcommon.Timestamp(startTimeNs + 1000000)) // +1000 μs
		childSpan.SetEndTimestamp(pcommon.Timestamp(startTimeNs + 4000000))   // +3000 μs duration
	}

	marshaler := &ptrace.JSONMarshaler{}
	data, err := marshaler.MarshalTraces(td)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func leftPad(s string, length int, pad byte) string {
	for len(s) < length {
		s = string(pad) + s
	}
	return s
}

func TestSampleTraces(t *testing.T) {
	// Create 1000 traces
	traces := make([]*tpackmodel.Trace, 1000)
	for i := range traces {
		traces[i] = &tpackmodel.Trace{
			TraceID: "trace" + string(rune(i)),
			Spans: map[string]*tpackmodel.Span{
				"span": {SpanID: "span", StartTime: int64(i) * 1000, Duration: 100,
					Feature: tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{"service.name": "svc", "operation.name": "op", "status.code": "0"})},
			},
		}
	}

	// Sample at rate 10 (keep ~1/10)
	rng := rand.New(rand.NewSource(42))
	sampled := sampleTraces(traces, 10, rng)

	// With 1000 traces and rate 10, expect ~100 (allow wide tolerance)
	ratio := float64(len(sampled)) / float64(len(traces))
	expected := 1.0 / 10.0
	if math.Abs(ratio-expected) > 0.1 {
		t.Errorf("sampling ratio = %.3f, want ~%.3f (got %d/%d)",
			ratio, expected, len(sampled), len(traces))
	}
	t.Logf("Sampled %d/1000 traces (ratio=%.3f)", len(sampled), ratio)
}

func TestSampleTracesDeterministic(t *testing.T) {
	traces := make([]*tpackmodel.Trace, 100)
	for i := range traces {
		traces[i] = &tpackmodel.Trace{
			TraceID: "trace" + string(rune(i)),
			Spans: map[string]*tpackmodel.Span{
				"span": {SpanID: "span", StartTime: int64(i) * 1000, Duration: 100,
					Feature: tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{"service.name": "svc", "operation.name": "op", "status.code": "0"})},
			},
		}
	}

	rng1 := rand.New(rand.NewSource(123))
	sampled1 := sampleTraces(traces, 5, rng1)

	rng2 := rand.New(rand.NewSource(123))
	sampled2 := sampleTraces(traces, 5, rng2)

	if len(sampled1) != len(sampled2) {
		t.Fatalf("determinism failed: got %d vs %d", len(sampled1), len(sampled2))
	}
	for i := range sampled1 {
		if sampled1[i].TraceID != sampled2[i].TraceID {
			t.Fatalf("determinism failed at index %d: %s vs %s",
				i, sampled1[i].TraceID, sampled2[i].TraceID)
		}
	}
}

func TestWriteSampledOTLP(t *testing.T) {
	tmpDir := t.TempDir()
	outputDir := filepath.Join(tmpDir, "dataset")

	buckets := map[int64][]*tpackmodel.Trace{
		16: {
			{
				TraceID: "00000000000000000000000000000001",
				Spans: map[string]*tpackmodel.Span{
					"00000000000001a0": {
						SpanID: "00000000000001a0", ParentSpanID: "",
						Feature:   tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{"service.name": "frontend", "operation.name": "/api", "status.code": "0"}),
						StartTime: 1000000000, Duration: 5000,
						Metadata: map[string]string{"http.status_code": "200"},
					},
					"00000000000001b0": {
						SpanID: "00000000000001b0", ParentSpanID: "00000000000001a0",
						Feature:   tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{"service.name": "backend", "operation.name": "SELECT", "status.code": "0"}),
						StartTime: 1000001000, Duration: 3000,
						Metadata: map[string]string{"http.status_code": "200"},
					},
				},
			},
		},
	}
	bucketKeys := []int64{16}

	if err := writeSampledOTLP(outputDir, bucketKeys, buckets); err != nil {
		t.Fatalf("writeSampledOTLP failed: %v", err)
	}

	// Verify chunked proto output exists and is valid OTLP
	chunkFiles, err := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunkFiles) == 0 {
		t.Fatal("expected at least one chunk_*.pb file")
	}

	unmarshaler := &ptrace.ProtoUnmarshaler{}
	spanCount := 0
	for _, path := range chunkFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		td, err := unmarshaler.UnmarshalTraces(data)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		for i := 0; i < td.ResourceSpans().Len(); i++ {
			for j := 0; j < td.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
				spanCount += td.ResourceSpans().At(i).ScopeSpans().At(j).Spans().Len()
			}
		}
	}
	if spanCount != 2 {
		t.Errorf("expected 2 spans, got %d", spanCount)
	}
}

func TestRunHeadSampling(t *testing.T) {
	tmpDir := t.TempDir()

	// Create synthetic OTLP input with 20 traces
	inputPath := makeTestInputOTLP(t, tmpDir, 20)
	outputDir := filepath.Join(tmpDir, "output")

	rates := []int{2, 5}
	iterations := 2
	seed := int64(42)
	bucketDurationUs := int64(60) * 1_000_000

	if err := runHeadSampling(inputPath, outputDir, rates, iterations, seed, bucketDurationUs, tpackmodel.DefaultFeatureColumns, []string{"http.status_code"}); err != nil {
		t.Fatalf("runHeadSampling failed: %v", err)
	}

	// Verify directory structure: head_{rate}_{iter}/ for each combination
	expectedDirs := []string{
		"head_2_1", "head_2_2",
		"head_5_1", "head_5_2",
	}
	for _, dir := range expectedDirs {
		dirPath := filepath.Join(outputDir, dir)

		// dataset/chunk_*.pb must exist (OTLP output)
		chunks, _ := filepath.Glob(filepath.Join(dirPath, "dataset", "chunk_*.pb"))
		if len(chunks) == 0 {
			t.Errorf("missing chunk_*.pb in %s/dataset", dirPath)
		}

		// compressed/data/ timing files must exist
		compCPUPath := filepath.Join(dirPath, "compressed", "data", "compression_cpu_time_seconds")
		if _, err := os.Stat(compCPUPath); os.IsNotExist(err) {
			t.Errorf("missing %s", compCPUPath)
		}

		// compressed/data/ must exist with at least one model_bucket_* file
		compressedDir := filepath.Join(dirPath, "compressed", "data")
		entries, err := os.ReadDir(compressedDir)
		if err != nil {
			t.Errorf("read compressed dir %s: %v", compressedDir, err)
			continue
		}
		hasBucket := false
		for _, e := range entries {
			if len(e.Name()) > len("model_bucket_") && e.Name()[:len("model_bucket_")] == "model_bucket_" {
				hasBucket = true
				break
			}
		}
		if !hasBucket {
			t.Errorf("no model_bucket_* files in %s", compressedDir)
		}
	}
}

func TestParseRates(t *testing.T) {
	tests := []struct {
		input   string
		want    []int
		wantErr bool
	}{
		{"2,10,150", []int{2, 10, 150}, false},
		{"5", []int{5}, false},
		{" 2 , 10 ", []int{2, 10}, false},
		{"", nil, true},
		{"abc", nil, true},
		{"0", nil, true},
		{"-1", nil, true},
	}

	for _, tt := range tests {
		got, err := parseRates(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseRates(%q) = %v, want error", tt.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRates(%q) error: %v", tt.input, err)
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("parseRates(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseRates(%q)[%d] = %d, want %d", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestConvertSampledToPdata(t *testing.T) {
	traces := []*tpackmodel.Trace{
		{
			TraceID: "00000000000000000000000000000001",
			Spans: map[string]*tpackmodel.Span{
				"00000000000001a0": {
					SpanID: "00000000000001a0", ParentSpanID: "",
					Feature:   tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{"service.name": "frontend", "operation.name": "/api", "status.code": "0"}),
					StartTime: 1000000000, Duration: 5000,
					Metadata: map[string]string{"http.status_code": "200"},
				},
			},
		},
	}

	td := convertSampledToPdata(traces)

	spanCount := 0
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		if sn, ok := rs.Resource().Attributes().Get("service.name"); ok {
			if sn.Str() != "frontend" {
				t.Errorf("service.name = %q, want frontend", sn.Str())
			}
		} else {
			t.Error("missing service.name attribute")
		}
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			spanCount += rs.ScopeSpans().At(j).Spans().Len()
		}
	}

	if spanCount != 1 {
		t.Errorf("expected 1 span, got %d", spanCount)
	}
}
