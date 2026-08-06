package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var testFeatureColumns = tpackmodel.DefaultFeatureColumns

// makeTestPtraceTraces creates ptrace.Traces matching the makeTestTraces() test data:
// Trace 1: frontend→backend (status ok, http 200)
// Trace 2: frontend→payment (payment has error status, http 500)
func makeTestPtraceTraces() ptrace.Traces {
	td := ptrace.NewTraces()

	// Trace 1: frontend service spans
	rs1 := td.ResourceSpans().AppendEmpty()
	rs1.Resource().Attributes().PutStr("service.name", "frontend")
	ss1 := rs1.ScopeSpans().AppendEmpty()

	// Root span: frontend GET /api
	span1a := ss1.Spans().AppendEmpty()
	span1a.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span1a.SetSpanID(hexToSpanID("00000000000001a0"))
	span1a.SetName("GET /api")
	span1a.Status().SetCode(ptrace.StatusCodeUnset)
	span1a.Attributes().PutStr("http.status_code", "200")
	span1a.Attributes().PutStr("http.method", "GET")
	span1a.SetStartTimestamp(pcommon.Timestamp(1000000000000)) // 1000000000 μs in ns
	span1a.SetEndTimestamp(pcommon.Timestamp(1000005000000))   // +5000 μs

	// Trace 2: frontend POST /checkout
	span2a := ss1.Spans().AppendEmpty()
	span2a.SetTraceID(hexToTraceID("00000000000000000000000000000002"))
	span2a.SetSpanID(hexToSpanID("00000000000002a0"))
	span2a.SetName("POST /checkout")
	span2a.Status().SetCode(ptrace.StatusCodeUnset)
	span2a.Attributes().PutStr("http.method", "POST")
	span2a.SetStartTimestamp(pcommon.Timestamp(1060000000000)) // 1060000000 μs in ns
	span2a.SetEndTimestamp(pcommon.Timestamp(1060008000000))   // +8000 μs

	// Trace 1: backend service spans
	rs2 := td.ResourceSpans().AppendEmpty()
	rs2.Resource().Attributes().PutStr("service.name", "backend")
	ss2 := rs2.ScopeSpans().AppendEmpty()

	span1b := ss2.Spans().AppendEmpty()
	span1b.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span1b.SetSpanID(hexToSpanID("00000000000001b0"))
	span1b.SetParentSpanID(hexToSpanID("00000000000001a0"))
	span1b.SetName("SELECT * FROM products")
	span1b.Status().SetCode(ptrace.StatusCodeUnset)
	span1b.Attributes().PutStr("http.status_code", "200")
	span1b.SetStartTimestamp(pcommon.Timestamp(1000001000000)) // 1000001000 μs
	span1b.SetEndTimestamp(pcommon.Timestamp(1000004000000))   // +3000 μs

	// Trace 2: payment service spans
	rs3 := td.ResourceSpans().AppendEmpty()
	rs3.Resource().Attributes().PutStr("service.name", "payment")
	ss3 := rs3.ScopeSpans().AppendEmpty()

	span2b := ss3.Spans().AppendEmpty()
	span2b.SetTraceID(hexToTraceID("00000000000000000000000000000002"))
	span2b.SetSpanID(hexToSpanID("00000000000002b0"))
	span2b.SetParentSpanID(hexToSpanID("00000000000002a0"))
	span2b.SetName("ProcessPayment")
	span2b.Status().SetCode(ptrace.StatusCodeError)
	span2b.Attributes().PutStr("http.status_code", "500")
	span2b.SetStartTimestamp(pcommon.Timestamp(1060001000000)) // 1060001000 μs
	span2b.SetEndTimestamp(pcommon.Timestamp(1060005000000))   // +4000 μs

	return td
}

func hexToTraceID(h string) pcommon.TraceID {
	var tid pcommon.TraceID
	otlpconv.HexToBytes(h, tid[:])
	return tid
}

func hexToSpanID(h string) pcommon.SpanID {
	var sid pcommon.SpanID
	otlpconv.HexToBytes(h, sid[:])
	return sid
}

func TestConvertFromPdata(t *testing.T) {
	td := makeTestPtraceTraces()
	traces := otlpconv.FromPdata(td, testFeatureColumns, []string{"http.status_code"})

	if len(traces) != 2 {
		t.Fatalf("expected 2 traces, got %d", len(traces))
	}

	traceMap := make(map[string]*tpackmodel.Trace)
	for _, tr := range traces {
		traceMap[tr.TraceID] = tr
	}

	// Trace 1: 2 spans
	t1 := traceMap["00000000000000000000000000000001"]
	if t1 == nil {
		t.Fatal("trace 1 not found")
	}
	if len(t1.Spans) != 2 {
		t.Fatalf("trace 1: expected 2 spans, got %d", len(t1.Spans))
	}

	root := t1.Spans["00000000000001a0"]
	if root == nil {
		t.Fatal("root span not found")
	}
	if root.ParentSpanID != "" {
		t.Errorf("root ParentSpanID = %q, want empty", root.ParentSpanID)
	}
	if root.StartTime != 1000000000 {
		t.Errorf("root StartTime = %d, want 1000000000", root.StartTime)
	}
	if root.Duration != 5000 {
		t.Errorf("root Duration = %d, want 5000", root.Duration)
	}
	if metadataGet(root.Metadata, "http.status_code") != "200" {
		t.Errorf("root http.status_code = %q, want 200", metadataGet(root.Metadata, "http.status_code"))
	}
	if root.Feature.ServiceName() != "frontend" {
		t.Errorf("root service = %q, want frontend", root.Feature.ServiceName())
	}
	if root.Feature.OperationName() != "GET /api" {
		t.Errorf("root OperationName = %q, want GET /api", root.Feature.OperationName())
	}

	// Child span
	child := t1.Spans["00000000000001b0"]
	if child == nil {
		t.Fatal("child span not found")
	}
	if child.ParentSpanID != "00000000000001a0" {
		t.Errorf("child ParentSpanID = %q, want 00000000000001a0", child.ParentSpanID)
	}
	if child.Duration != 3000 {
		t.Errorf("child Duration = %d, want 3000", child.Duration)
	}

	// Trace 2: error trace
	t2 := traceMap["00000000000000000000000000000002"]
	if t2 == nil {
		t.Fatal("trace 2 not found")
	}
	errorSpan := t2.Spans["00000000000002b0"]
	if errorSpan == nil {
		t.Fatal("error span not found")
	}
	if metadataGet(errorSpan.Metadata, "http.status_code") != "500" {
		t.Errorf("error span http.status_code = %q, want 500", metadataGet(errorSpan.Metadata, "http.status_code"))
	}
	if errorSpan.Feature.StatusCode() != "2" {
		t.Errorf("error span Feature.StatusCode = %q, want 2", errorSpan.Feature.StatusCode())
	}
}

func TestReadOTLPProtoBinary(t *testing.T) {
	td := makeTestPtraceTraces()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "traces.pb")
	marshaler := &ptrace.ProtoMarshaler{}
	data, err := marshaler.MarshalTraces(td)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	bucketDurationUs := int64(60) * 1_000_000
	buckets, err := readOTLP(path, bucketDurationUs, testFeatureColumns, []string{"http.status_code"})
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
}

func TestReadOTLPJSON(t *testing.T) {
	td := makeTestPtraceTraces()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "traces.json")
	marshaler := &ptrace.JSONMarshaler{}
	data, err := marshaler.MarshalTraces(td)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	bucketDurationUs := int64(60) * 1_000_000
	buckets, err := readOTLP(path, bucketDurationUs, testFeatureColumns, []string{"http.status_code"})
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
}

func TestWriteOutputOTLP(t *testing.T) {
	fc := testFeatureColumns
	buckets := map[int64][]*tpackmodel.Trace{
		16: {
			{
				TraceID: "trace1",
				Spans: map[string]*tpackmodel.Span{
					"span1a": {
						SpanID: "span1a", ParentSpanID: "",
						Feature:   tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "frontend", "operation.name": "/api", "status.code": "0"}),
						StartTime: 1000000000, Duration: 5000,
						Metadata: map[string]string{"http.status_code": "200"},
					},
					"span1b": {
						SpanID: "span1b", ParentSpanID: "span1a",
						Feature:   tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "backend", "operation.name": "SELECT", "status.code": "0"}),
						StartTime: 1000001000, Duration: 3000,
						Metadata: map[string]string{"http.status_code": "200"},
					},
				},
			},
		},
	}
	bucketKeys := []int64{16}

	config := defaultTestConfig()
	results := make([]bucketResult, 1)
	results[0] = processBucket(16, buckets[16], config, fc, nil)

	if len(results[0].Spans) == 0 {
		t.Fatal("expected generated spans")
	}

	tmpDir := t.TempDir()
	outputDir := filepath.Join(tmpDir, "output")
	if err := writeOutputOTLP(outputDir, bucketKeys, results); err != nil {
		t.Fatalf("writeOutputOTLP failed: %v", err)
	}

	// Writer now produces chunk_*.pb files instead of a single traces.pb
	chunkFiles, err := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if err != nil {
		t.Fatalf("glob chunks: %v", err)
	}
	if len(chunkFiles) == 0 {
		t.Fatal("expected at least one chunk_*.pb file")
	}

	unmarshaler := &ptrace.ProtoUnmarshaler{}
	spanCount := 0
	serviceSet := make(map[string]struct{})
	for _, pbPath := range chunkFiles {
		data, err := os.ReadFile(pbPath)
		if err != nil {
			t.Fatalf("read %s: %v", pbPath, err)
		}
		outTd, err := unmarshaler.UnmarshalTraces(data)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", pbPath, err)
		}
		for i := 0; i < outTd.ResourceSpans().Len(); i++ {
			rs := outTd.ResourceSpans().At(i)
			if sn, ok := rs.Resource().Attributes().Get("service.name"); ok {
				serviceSet[sn.Str()] = struct{}{}
			}
			for j := 0; j < rs.ScopeSpans().Len(); j++ {
				spanCount += rs.ScopeSpans().At(j).Spans().Len()
			}
		}
	}

	if spanCount == 0 {
		t.Fatal("expected spans in output OTLP")
	}
	t.Logf("OTLP output: %d spans across %d services", spanCount, len(serviceSet))

	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}
	sort.Strings(services)
	t.Logf("Services: %v", services)
}

func TestOTLPRoundTrip(t *testing.T) {
	td := makeTestPtraceTraces()

	tmpDir := t.TempDir()
	inputPath := filepath.Join(tmpDir, "input.pb")

	marshaler := &ptrace.ProtoMarshaler{}
	data, err := marshaler.MarshalTraces(td)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	bucketDurationUs := int64(60) * 1_000_000
	buckets, err := readOTLP(inputPath, bucketDurationUs, testFeatureColumns, []string{"http.status_code"})
	if err != nil {
		t.Fatalf("readOTLP: %v", err)
	}

	config := defaultTestConfig()
	bucketKeys := make([]int64, 0, len(buckets))
	for k := range buckets {
		bucketKeys = append(bucketKeys, k)
	}
	slices.Sort(bucketKeys)

	results := make([]bucketResult, len(bucketKeys))
	for i, bk := range bucketKeys {
		results[i] = processBucket(bk, buckets[bk], config, testFeatureColumns, nil)
	}

	outputDir := filepath.Join(tmpDir, "output")
	if err := writeOutputOTLP(outputDir, bucketKeys, results); err != nil {
		t.Fatalf("writeOutputOTLP: %v", err)
	}

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
		outputData, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		outTd, err := unmarshaler.UnmarshalTraces(outputData)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < outTd.ResourceSpans().Len(); i++ {
			for j := 0; j < outTd.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
				spanCount += outTd.ResourceSpans().At(i).ScopeSpans().At(j).Spans().Len()
			}
		}
	}

	if spanCount == 0 {
		t.Fatal("round-trip produced 0 spans")
	}
	t.Logf("OTLP round-trip: %d output spans", spanCount)
}

func TestReadOTLPChunkedDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	chunkDir := filepath.Join(tmpDir, "chunks")
	if err := os.MkdirAll(chunkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	marshaler := &ptrace.JSONMarshaler{}

	// Chunk 1: trace 1
	td1 := ptrace.NewTraces()
	rs1 := td1.ResourceSpans().AppendEmpty()
	rs1.Resource().Attributes().PutStr("service.name", "frontend")
	ss1 := rs1.ScopeSpans().AppendEmpty()
	span1 := ss1.Spans().AppendEmpty()
	span1.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span1.SetSpanID(hexToSpanID("00000000000001a0"))
	span1.SetName("GET /api")
	span1.Attributes().PutStr("http.status_code", "200")
	span1.SetStartTimestamp(pcommon.Timestamp(1000000000000))
	span1.SetEndTimestamp(pcommon.Timestamp(1000005000000))
	data1, _ := marshaler.MarshalTraces(td1)
	os.WriteFile(filepath.Join(chunkDir, "chunk_00000000000000000000.json"), data1, 0o644)

	// Chunk 2: trace 2
	td2 := ptrace.NewTraces()
	rs2 := td2.ResourceSpans().AppendEmpty()
	rs2.Resource().Attributes().PutStr("service.name", "payment")
	ss2 := rs2.ScopeSpans().AppendEmpty()
	span2 := ss2.Spans().AppendEmpty()
	span2.SetTraceID(hexToTraceID("00000000000000000000000000000002"))
	span2.SetSpanID(hexToSpanID("00000000000002b0"))
	span2.SetName("ProcessPayment")
	span2.Status().SetCode(ptrace.StatusCodeError)
	span2.Attributes().PutStr("http.status_code", "500")
	span2.SetStartTimestamp(pcommon.Timestamp(2000000000000))
	span2.SetEndTimestamp(pcommon.Timestamp(2000005000000))
	data2, _ := marshaler.MarshalTraces(td2)
	os.WriteFile(filepath.Join(chunkDir, "chunk_00000000000000000001.json"), data2, 0o644)

	bucketDurationUs := int64(60) * 1_000_000
	buckets, err := readOTLP(chunkDir, bucketDurationUs, testFeatureColumns, []string{"http.status_code"})
	if err != nil {
		t.Fatalf("readOTLP on directory failed: %v", err)
	}

	totalTraces := 0
	totalSpans := 0
	for _, traces := range buckets {
		totalTraces += len(traces)
		for _, tr := range traces {
			totalSpans += len(tr.Spans)
		}
	}

	if totalTraces != 2 {
		t.Errorf("expected 2 traces, got %d", totalTraces)
	}
	if totalSpans != 2 {
		t.Errorf("expected 2 spans, got %d", totalSpans)
	}
}

func TestHexToByteSlice(t *testing.T) {
	dst := make([]byte, 4)
	otlpconv.HexToBytes("deadbeef", dst)
	expected := []byte{0xde, 0xad, 0xbe, 0xef}
	for i, b := range expected {
		if dst[i] != b {
			t.Errorf("byte[%d] = %02x, want %02x", i, dst[i], b)
		}
	}
}

func defaultTestConfig() tpackmodel.TPackConfig {
	config := tpackmodel.DefaultConfig()
	config.RandomSeed = 42
	return config
}
