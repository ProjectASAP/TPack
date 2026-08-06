package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// TestRoundTrip verifies: OTLP input → compress → write OTLP → verify output.
func TestRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := filepath.Join(tmpDir, "traces.json")
	outputDir := filepath.Join(tmpDir, "output")

	// Create OTLP test input using pdata API
	td := makeTestPtraceTraces()
	marshaler := &ptrace.JSONMarshaler{}
	data, err := marshaler.MarshalTraces(td)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Read and bucket
	bucketDurationUs := int64(60) * 1_000_000
	dependentAttributes := []string{"http.status_code"}
	primaryAttributes := tpackmodel.DefaultFeatureColumns
	buckets, err := readOTLP(inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
	if err != nil {
		t.Fatalf("readOTLP failed: %v", err)
	}

	totalTraces := 0
	for _, traces := range buckets {
		totalTraces += len(traces)
	}
	if totalTraces != 2 {
		t.Fatalf("expected 2 traces, got %d", totalTraces)
	}

	// Process each bucket
	config := tpackmodel.DefaultConfig()
	config.RandomSeed = 42

	var bucketKeys []int64
	for k := range buckets {
		bucketKeys = append(bucketKeys, k)
	}

	results := make([]bucketResult, len(bucketKeys))
	for i, bk := range bucketKeys {
		results[i] = processBucket(bk, buckets[bk], config, primaryAttributes, dependentAttributes)
	}

	// Verify we got some output spans
	totalOutputSpans := 0
	for _, r := range results {
		totalOutputSpans += len(r.Spans)
	}
	if totalOutputSpans == 0 {
		t.Fatal("expected generated spans, got 0")
	}
	t.Logf("Generated %d spans from %d input traces", totalOutputSpans, totalTraces)

	// Write OTLP output
	if err := writeOutputOTLP(outputDir, bucketKeys, results); err != nil {
		t.Fatalf("writeOutputOTLP failed: %v", err)
	}
	if err := writeTimingFiles(outputDir, 1.2, 0, 0.3, 0, 10, 100); err != nil {
		t.Fatalf("writeTimingFiles failed: %v", err)
	}

	// Verify output OTLP chunks exist and are valid
	chunkFiles, err := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if err != nil {
		t.Fatalf("glob chunks: %v", err)
	}
	if len(chunkFiles) == 0 {
		t.Fatal("expected at least one chunk_*.pb file")
	}

	unmarshaler := &ptrace.ProtoUnmarshaler{}
	spanCount := 0
	for _, path := range chunkFiles {
		outData, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		outTd, err := unmarshaler.UnmarshalTraces(outData)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		for i := 0; i < outTd.ResourceSpans().Len(); i++ {
			for j := 0; j < outTd.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
				spanCount += outTd.ResourceSpans().At(i).ScopeSpans().At(j).Spans().Len()
			}
		}
	}
	if spanCount == 0 {
		t.Fatal("expected spans in output, got 0")
	}

	// Verify timing files (4 canonical: compression/decompression × cpu/gpu)
	compCPUBytes, err := os.ReadFile(filepath.Join(outputDir, "compression_cpu_time_seconds"))
	if err != nil {
		t.Fatalf("read compression_cpu timing: %v", err)
	}
	if !strings.Contains(string(compCPUBytes), "1.2") {
		t.Errorf("expected compression_cpu timing ~1.2, got %s", compCPUBytes)
	}

	decompCPUBytes, err := os.ReadFile(filepath.Join(outputDir, "decompression_cpu_time_seconds"))
	if err != nil {
		t.Fatalf("read decompression_cpu timing: %v", err)
	}
	if !strings.Contains(string(decompCPUBytes), "0.3") {
		t.Errorf("expected decompression_cpu timing ~0.3, got %s", decompCPUBytes)
	}

	t.Logf("Output OTLP has %d spans", spanCount)
}
