package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func TestRunTransform(t *testing.T) {
	// Create a JSONL directory with 2 files
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	marshaler := &ptrace.JSONMarshaler{}

	// File 1: trace with frontend spans
	td1 := ptrace.NewTraces()
	rs1 := td1.ResourceSpans().AppendEmpty()
	rs1.Resource().Attributes().PutStr("service.name", "frontend")
	ss1 := rs1.ScopeSpans().AppendEmpty()
	span1 := ss1.Spans().AppendEmpty()
	span1.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span1.SetSpanID(hexToSpanID("00000000000001a0"))
	span1.SetName("GET /api")
	span1.Attributes().PutStr("http.method", "GET")
	span1.Attributes().PutStr("http.status_code", "200")
	span1.Attributes().PutStr("http.url", "http://example.com/api") // should be pruned
	span1.SetStartTimestamp(pcommon.Timestamp(1000000000000))
	span1.SetEndTimestamp(pcommon.Timestamp(1000005000000))

	data1, _ := marshaler.MarshalTraces(td1)
	if err := os.WriteFile(filepath.Join(inputDir, "file1.json"), data1, 0o644); err != nil {
		t.Fatal(err)
	}

	// File 2: trace with backend spans (same trace as file 1, same 30-min window)
	td2 := ptrace.NewTraces()
	rs2 := td2.ResourceSpans().AppendEmpty()
	rs2.Resource().Attributes().PutStr("service.name", "backend")
	ss2 := rs2.ScopeSpans().AppendEmpty()
	span2 := ss2.Spans().AppendEmpty()
	span2.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span2.SetSpanID(hexToSpanID("00000000000001b0"))
	span2.SetParentSpanID(hexToSpanID("00000000000001a0"))
	span2.SetName("SELECT * FROM products")
	span2.Attributes().PutStr("http.status_code", "200")
	span2.Attributes().PutStr("db.statement", "SELECT * FROM products") // should be pruned
	span2.SetStartTimestamp(pcommon.Timestamp(1000001000000))
	span2.SetEndTimestamp(pcommon.Timestamp(1000004000000))

	data2, _ := marshaler.MarshalTraces(td2)
	if err := os.WriteFile(filepath.Join(inputDir, "file2.json"), data2, 0o644); err != nil {
		t.Fatal(err)
	}

	// Run transform — output is now a directory of chunk files
	outputDir := filepath.Join(tmpDir, "output")
	if err := runTransform(inputDir, outputDir, nil, []string{"http.status_code"}, 0, 0, false, 60_000_000); err != nil {
		t.Fatalf("runTransform failed: %v", err)
	}

	// Verify output is a directory with chunk files
	chunkFiles, err := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if err != nil {
		t.Fatalf("glob chunks: %v", err)
	}
	if len(chunkFiles) == 0 {
		t.Fatal("expected at least one chunk file")
	}

	// Read all chunks and combine
	unmarshaler := &ptrace.ProtoUnmarshaler{}
	totalSpans := 0
	for _, chunkFile := range chunkFiles {
		outData, err := os.ReadFile(chunkFile)
		if err != nil {
			t.Fatalf("read chunk: %v", err)
		}
		outTd, err := unmarshaler.UnmarshalTraces(outData)
		if err != nil {
			t.Fatalf("unmarshal chunk %s: %v", chunkFile, err)
		}

		for i := 0; i < outTd.ResourceSpans().Len(); i++ {
			rs := outTd.ResourceSpans().At(i)
			for j := 0; j < rs.ScopeSpans().Len(); j++ {
				ss := rs.ScopeSpans().At(j)
				for k := 0; k < ss.Spans().Len(); k++ {
					span := ss.Spans().At(k)
					totalSpans++

					// http.url and db.statement should have been pruned
					if _, ok := span.Attributes().Get("http.url"); ok {
						t.Error("http.url should have been pruned")
					}
					if _, ok := span.Attributes().Get("db.statement"); ok {
						t.Error("db.statement should have been pruned")
					}

					// http.status_code and http.method should be kept
					if v, ok := span.Attributes().Get("http.status_code"); ok {
						if v.AsString() != "200" {
							t.Errorf("http.status_code = %q, want 200", v.AsString())
						}
					}
				}
			}
		}
	}

	if totalSpans != 2 {
		t.Errorf("expected 2 spans, got %d", totalSpans)
	}
}

func TestRunTransformMultipleChunks(t *testing.T) {
	// Create traces in two different 30-min windows to verify multiple chunk files
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	marshaler := &ptrace.JSONMarshaler{}

	// Trace 1: timestamp in chunk 0
	td1 := ptrace.NewTraces()
	rs1 := td1.ResourceSpans().AppendEmpty()
	rs1.Resource().Attributes().PutStr("service.name", "svc-a")
	ss1 := rs1.ScopeSpans().AppendEmpty()
	span1 := ss1.Spans().AppendEmpty()
	span1.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span1.SetSpanID(hexToSpanID("0000000000000001"))
	span1.SetName("op1")
	span1.SetStartTimestamp(pcommon.Timestamp(100_000_000_000)) // 100s in ns
	span1.SetEndTimestamp(pcommon.Timestamp(100_001_000_000))

	data1, _ := marshaler.MarshalTraces(td1)
	if err := os.WriteFile(filepath.Join(inputDir, "file1.json"), data1, 0o644); err != nil {
		t.Fatal(err)
	}

	// Trace 2: timestamp 31 minutes later → different 30-min chunk
	td2 := ptrace.NewTraces()
	rs2 := td2.ResourceSpans().AppendEmpty()
	rs2.Resource().Attributes().PutStr("service.name", "svc-b")
	ss2 := rs2.ScopeSpans().AppendEmpty()
	span2 := ss2.Spans().AppendEmpty()
	span2.SetTraceID(hexToTraceID("00000000000000000000000000000002"))
	span2.SetSpanID(hexToSpanID("0000000000000002"))
	span2.SetName("op2")
	// 31 minutes = 1860 seconds → 1_860_000_000_000 ns
	span2.SetStartTimestamp(pcommon.Timestamp(1_860_000_000_000))
	span2.SetEndTimestamp(pcommon.Timestamp(1_860_001_000_000))

	data2, _ := marshaler.MarshalTraces(td2)
	if err := os.WriteFile(filepath.Join(inputDir, "file2.json"), data2, 0o644); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(tmpDir, "output")
	if err := runTransform(inputDir, outputDir, nil, nil, 0, 0, false, 60_000_000); err != nil {
		t.Fatalf("runTransform failed: %v", err)
	}

	chunkFiles, err := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(chunkFiles) != 2 {
		t.Fatalf("expected 2 chunk files, got %d", len(chunkFiles))
	}

	// Verify each chunk has exactly 1 span
	unmarshaler := &ptrace.ProtoUnmarshaler{}
	for _, f := range chunkFiles {
		data, _ := os.ReadFile(f)
		td, err := unmarshaler.UnmarshalTraces(data)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", f, err)
		}
		if countPdataSpans(td) != 1 {
			t.Errorf("chunk %s: expected 1 span, got %d", filepath.Base(f), countPdataSpans(td))
		}
	}
}

func TestAdjustDurations(t *testing.T) {
	// Create traces where child extends beyond parent
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	ss := rs.ScopeSpans().AppendEmpty()

	// Parent: starts at 1000, ends at 2000 (duration 1000ns)
	parent := ss.Spans().AppendEmpty()
	parent.SetSpanID(hexToSpanID("0000000000000001"))
	parent.SetStartTimestamp(pcommon.Timestamp(1000))
	parent.SetEndTimestamp(pcommon.Timestamp(2000))

	// Child: starts at 1100, ends at 3000 — extends beyond parent
	child := ss.Spans().AppendEmpty()
	child.SetSpanID(hexToSpanID("0000000000000002"))
	child.SetParentSpanID(hexToSpanID("0000000000000001"))
	child.SetStartTimestamp(pcommon.Timestamp(1100))
	child.SetEndTimestamp(pcommon.Timestamp(3000))

	// Build spanIndex and compute adjustments via unified path
	spanIndex := map[pcommon.SpanID]compactSpan{
		parent.SpanID(): {parentID: parent.ParentSpanID(), start: parent.StartTimestamp(), end: parent.EndTimestamp()},
		child.SpanID():  {parentID: child.ParentSpanID(), start: child.StartTimestamp(), end: child.EndTimestamp()},
	}
	adjustments := computeDurationAdjustments(spanIndex)
	applyDurationAdjustments(td, adjustments)

	// Parent end should now be 3000 (expanded to cover child)
	if parent.EndTimestamp() != 3000 {
		t.Errorf("parent EndTimestamp = %d, want 3000", parent.EndTimestamp())
	}
	// Parent start should remain 1000
	if parent.StartTimestamp() != 1000 {
		t.Errorf("parent StartTimestamp = %d, want 1000", parent.StartTimestamp())
	}
}

func TestAdjustDurationsChildStartsBeforeParent(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	ss := rs.ScopeSpans().AppendEmpty()

	parent := ss.Spans().AppendEmpty()
	parent.SetSpanID(hexToSpanID("0000000000000001"))
	parent.SetStartTimestamp(pcommon.Timestamp(2000))
	parent.SetEndTimestamp(pcommon.Timestamp(5000))

	child := ss.Spans().AppendEmpty()
	child.SetSpanID(hexToSpanID("0000000000000002"))
	child.SetParentSpanID(hexToSpanID("0000000000000001"))
	child.SetStartTimestamp(pcommon.Timestamp(1000)) // before parent
	child.SetEndTimestamp(pcommon.Timestamp(3000))

	spanIndex := map[pcommon.SpanID]compactSpan{
		parent.SpanID(): {parentID: parent.ParentSpanID(), start: parent.StartTimestamp(), end: parent.EndTimestamp()},
		child.SpanID():  {parentID: child.ParentSpanID(), start: child.StartTimestamp(), end: child.EndTimestamp()},
	}
	adjustments := computeDurationAdjustments(spanIndex)
	applyDurationAdjustments(td, adjustments)

	// Parent start should be adjusted to 1000
	if parent.StartTimestamp() != 1000 {
		t.Errorf("parent StartTimestamp = %d, want 1000", parent.StartTimestamp())
	}
}

func TestTransformCSV(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a small RE2-format CSV with 2 traces, 4 spans
	csvContent := `time,traceID,spanID,serviceName,methodName,operationName,startTimeMillis,startTime,duration,statusCode,parentSpanID,customTag
21:00,aaaa000000000000bbbb000000000001,0000000000000a01,frontend,GET,GET /api,1705353846000,1705353846000000,5000,0.0,,region=us
21:00,aaaa000000000000bbbb000000000001,0000000000000a02,backend,Query,SELECT products,1705353846001,1705353846001000,3000,0.0,0000000000000a01,region=eu
21:01,aaaa000000000000bbbb000000000002,0000000000000b01,frontend,POST,POST /checkout,1705353900000,1705353900000000,8000,0.0,,region=us
21:01,aaaa000000000000bbbb000000000002,0000000000000b02,payment,Charge,ProcessPayment,1705353900001,1705353900001000,4000,2.0,0000000000000b01,region=eu
`
	csvPath := filepath.Join(tmpDir, "traces.csv")
	if err := os.WriteFile(csvPath, []byte(csvContent), 0o644); err != nil {
		t.Fatal(err)
	}

	td, err := readCSVFile(csvPath)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify: 4 spans across 3 services (frontend, backend, payment)
	totalSpans := 0
	serviceSet := make(map[string]bool)
	spansByID := make(map[string]ptrace.Span)

	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		svc := ""
		if v, ok := rs.Resource().Attributes().Get("service.name"); ok {
			svc = v.Str()
			serviceSet[svc] = true
		}
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				totalSpans++
				sid := span.SpanID()
				sidHex := hex.EncodeToString(sid[:])
				spansByID[sidHex] = span
			}
		}
	}

	if totalSpans != 4 {
		t.Errorf("expected 4 spans, got %d", totalSpans)
	}
	for _, svc := range []string{"frontend", "backend", "payment"} {
		if !serviceSet[svc] {
			t.Errorf("missing service %q", svc)
		}
	}

	// Check a specific span: payment/ProcessPayment should have error status
	paymentSpan, ok := spansByID["0000000000000b02"]
	if !ok {
		t.Fatal("payment span 0000000000000b02 not found")
	}
	if paymentSpan.Status().Code() != ptrace.StatusCodeError {
		t.Errorf("payment span status = %v, want Error", paymentSpan.Status().Code())
	}
	if paymentSpan.Name() != "ProcessPayment" {
		t.Errorf("payment span name = %q, want ProcessPayment", paymentSpan.Name())
	}

	// Check method.name attribute (methodName from CSV stored as method.name)
	if v, ok := paymentSpan.Attributes().Get("method.name"); !ok || v.Str() != "Charge" {
		t.Errorf("payment span method.name = %q, want Charge", v.Str())
	}

	// Check extra attribute (customTag)
	if v, ok := paymentSpan.Attributes().Get("customTag"); !ok || v.Str() != "region=eu" {
		t.Errorf("payment span customTag = %q, want region=eu", v.Str())
	}

	// Check timestamps on first span (startTime=1705353846000000 μs → ns)
	frontendSpan, ok := spansByID["0000000000000a01"]
	if !ok {
		t.Fatal("frontend span 0000000000000a01 not found")
	}
	expectedStart := pcommon.Timestamp(1705353846000000 * 1000)
	expectedEnd := pcommon.Timestamp((1705353846000000 + 5000) * 1000)
	if frontendSpan.StartTimestamp() != expectedStart {
		t.Errorf("frontend span start = %d, want %d", frontendSpan.StartTimestamp(), expectedStart)
	}
	if frontendSpan.EndTimestamp() != expectedEnd {
		t.Errorf("frontend span end = %d, want %d", frontendSpan.EndTimestamp(), expectedEnd)
	}

	// Check parent span ID on child
	if paymentSpan.ParentSpanID() != hexToSpanID("0000000000000b01") {
		t.Errorf("payment span parent = %v, want 0000000000000b01", paymentSpan.ParentSpanID())
	}

	// Check unset status on a non-error span
	if frontendSpan.Status().Code() != ptrace.StatusCodeUnset {
		t.Errorf("frontend span status = %v, want Unset", frontendSpan.Status().Code())
	}
}

func TestTransformCSVMissingColumn(t *testing.T) {
	tmpDir := t.TempDir()
	csvContent := "traceID,spanID\nabc,def\n"
	csvPath := filepath.Join(tmpDir, "bad.csv")
	os.WriteFile(csvPath, []byte(csvContent), 0o644)

	_, err := readCSVFile(csvPath)
	if err == nil {
		t.Fatal("expected error for missing required columns")
	}
}

func TestRunTransformCSVAutoDetect(t *testing.T) {
	tmpDir := t.TempDir()

	csvContent := `time,traceID,spanID,serviceName,methodName,operationName,startTimeMillis,startTime,duration,statusCode,parentSpanID
21:00,aaaa000000000000bbbb000000000001,0000000000000a01,svc,GET,GET /api,0,1000000,500,0.0,
`
	csvPath := filepath.Join(tmpDir, "traces.csv")
	os.WriteFile(csvPath, []byte(csvContent), 0o644)

	outputDir := filepath.Join(tmpDir, "output")
	// runTransform should auto-detect CSV and produce chunked dir
	if err := runTransform(csvPath, outputDir, nil, nil, 0, 0, false, 60_000_000); err != nil {
		t.Fatalf("runTransform with CSV failed: %v", err)
	}

	chunks, _ := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if len(chunks) == 0 {
		t.Fatal("expected chunk files in output dir")
	}
	data, _ := os.ReadFile(chunks[0])
	unmarshaler := &ptrace.ProtoUnmarshaler{}
	td, err := unmarshaler.UnmarshalTraces(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if countPdataSpans(td) != 1 {
		t.Errorf("expected 1 span, got %d", countPdataSpans(td))
	}
}

func TestReadJaegerFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Write a minimal Jaeger JSON file (one trace, two spans)
	jaegerJSON := `{
		"data": [{
			"traceID": "abcdef1234567890",
			"spans": [
				{
					"spanID": "0000000000000001",
					"operationName": "root-op",
					"startTime": 1000000,
					"duration": 5000,
					"processID": "p1",
					"references": [],
					"tags": [{"key": "error", "value": false}]
				},
				{
					"spanID": "0000000000000002",
					"operationName": "child-op",
					"startTime": 1001000,
					"duration": 2000,
					"processID": "p2",
					"references": [{"refType": "CHILD_OF", "spanID": "0000000000000001"}],
					"tags": [{"key": "grpc.status", "value": "OK"}]
				}
			],
			"processes": {
				"p1": {"serviceName": "frontend"},
				"p2": {"serviceName": "backend"}
			}
		}]
	}`
	path := filepath.Join(tmpDir, "trace1.json")
	os.WriteFile(path, []byte(jaegerJSON), 0o644)

	td, err := readJaegerFile(path)
	if err != nil {
		t.Fatalf("readJaegerFile failed: %v", err)
	}

	// Should have 2 spans across 2 services
	if countPdataSpans(td) != 2 {
		t.Errorf("expected 2 spans, got %d", countPdataSpans(td))
	}

	// Verify original timestamps are preserved (not shifted)
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				startUs := int64(span.StartTimestamp()) / 1000
				if startUs != 1000000 && startUs != 1001000 {
					t.Errorf("unexpected start time %d µs (expected 1000000 or 1001000)", startUs)
				}
			}
		}
	}
}

func TestApplyTimeShifts(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()

	span := ss.Spans().AppendEmpty()
	tid := hexToTraceID("00000000000000000000000000000001")
	span.SetTraceID(tid)
	span.SetStartTimestamp(pcommon.Timestamp(100_000_000)) // 100ms in ns
	span.SetEndTimestamp(pcommon.Timestamp(200_000_000))   // 200ms in ns

	shifts := map[pcommon.TraceID]int64{
		tid: 50_000_000, // shift forward 50ms in ns
	}
	applyTimeShifts(td, shifts)

	if span.StartTimestamp() != 150_000_000 {
		t.Errorf("StartTimestamp = %d, want 150000000", span.StartTimestamp())
	}
	if span.EndTimestamp() != 250_000_000 {
		t.Errorf("EndTimestamp = %d, want 250000000", span.EndTimestamp())
	}
}

func TestRemapDiscardsLongTraces(t *testing.T) {
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	os.MkdirAll(inputDir, 0o755)

	marshaler := &ptrace.JSONMarshaler{}

	// Trace 1: short (10s duration) — should survive remap
	td1 := ptrace.NewTraces()
	rs1 := td1.ResourceSpans().AppendEmpty()
	rs1.Resource().Attributes().PutStr("service.name", "svc")
	ss1 := rs1.ScopeSpans().AppendEmpty()
	span1 := ss1.Spans().AppendEmpty()
	span1.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span1.SetSpanID(hexToSpanID("0000000000000001"))
	span1.SetName("short-op")
	span1.SetStartTimestamp(pcommon.Timestamp(1_000_000_000_000)) // 1000s in ns
	span1.SetEndTimestamp(pcommon.Timestamp(1_010_000_000_000))   // 1010s in ns (10s duration)
	data1, _ := marshaler.MarshalTraces(td1)
	os.WriteFile(filepath.Join(inputDir, "001.json"), data1, 0o644)

	// Trace 2: long (120s duration) — should be discarded by remap
	td2 := ptrace.NewTraces()
	rs2 := td2.ResourceSpans().AppendEmpty()
	rs2.Resource().Attributes().PutStr("service.name", "svc")
	ss2 := rs2.ScopeSpans().AppendEmpty()
	span2 := ss2.Spans().AppendEmpty()
	span2.SetTraceID(hexToTraceID("00000000000000000000000000000002"))
	span2.SetSpanID(hexToSpanID("0000000000000002"))
	span2.SetName("long-op")
	span2.SetStartTimestamp(pcommon.Timestamp(2_000_000_000_000)) // 2000s in ns
	span2.SetEndTimestamp(pcommon.Timestamp(2_120_000_000_000))   // 2120s in ns (120s duration)
	data2, _ := marshaler.MarshalTraces(td2)
	os.WriteFile(filepath.Join(inputDir, "002.json"), data2, 0o644)

	outputDir := filepath.Join(tmpDir, "output")
	if err := runTransform(inputDir, outputDir, nil, nil, 0, 0, true, 60_000_000); err != nil {
		t.Fatalf("runTransform with remap failed: %v", err)
	}

	// Read output — only the short trace should be present
	chunks, _ := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	if len(chunks) == 0 {
		t.Fatal("expected chunk files")
	}

	totalSpans := 0
	for _, chunk := range chunks {
		data, _ := os.ReadFile(chunk)
		um := &ptrace.ProtoUnmarshaler{}
		td, err := um.UnmarshalTraces(data)
		if err != nil {
			t.Fatalf("unmarshal chunk: %v", err)
		}
		totalSpans += countPdataSpans(td)
	}

	if totalSpans != 1 {
		t.Errorf("expected 1 span (short trace only), got %d", totalSpans)
	}
}

func TestRemapTimestampsInRange(t *testing.T) {
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	os.MkdirAll(inputDir, 0o755)

	marshaler := &ptrace.JSONMarshaler{}

	// A trace with 2 spans, 5s total duration, starting at time 999999s
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	ss := rs.ScopeSpans().AppendEmpty()

	root := ss.Spans().AppendEmpty()
	root.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	root.SetSpanID(hexToSpanID("0000000000000001"))
	root.SetName("root")
	root.SetStartTimestamp(pcommon.Timestamp(999_999_000_000_000_000)) // far in the future
	root.SetEndTimestamp(pcommon.Timestamp(999_999_005_000_000_000))   // +5s

	child := ss.Spans().AppendEmpty()
	child.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	child.SetSpanID(hexToSpanID("0000000000000002"))
	child.SetParentSpanID(hexToSpanID("0000000000000001"))
	child.SetName("child")
	child.SetStartTimestamp(pcommon.Timestamp(999_999_001_000_000_000)) // +1s from root
	child.SetEndTimestamp(pcommon.Timestamp(999_999_003_000_000_000))   // +3s from root

	data, _ := marshaler.MarshalTraces(td)
	os.WriteFile(filepath.Join(inputDir, "001.json"), data, 0o644)

	outputDir := filepath.Join(tmpDir, "output")
	if err := runTransform(inputDir, outputDir, nil, nil, 0, 0, true, 60_000_000); err != nil {
		t.Fatalf("runTransform with remap failed: %v", err)
	}

	// All spans should now be within [0, 60s)
	chunks, _ := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	um := &ptrace.ProtoUnmarshaler{}
	for _, chunk := range chunks {
		chunkData, _ := os.ReadFile(chunk)
		chunkTD, _ := um.UnmarshalTraces(chunkData)
		for i := 0; i < chunkTD.ResourceSpans().Len(); i++ {
			crs := chunkTD.ResourceSpans().At(i)
			for j := 0; j < crs.ScopeSpans().Len(); j++ {
				css := crs.ScopeSpans().At(j)
				for k := 0; k < css.Spans().Len(); k++ {
					s := css.Spans().At(k)
					startUs := int64(s.StartTimestamp()) / 1000
					endUs := int64(s.EndTimestamp()) / 1000
					if startUs < 0 || startUs >= 60_000_000 {
						t.Errorf("span %s start %d µs outside [0, 60s)", s.Name(), startUs)
					}
					if endUs < 0 || endUs > 60_000_000 {
						t.Errorf("span %s end %d µs outside [0, 60s]", s.Name(), endUs)
					}
				}
			}
		}
	}
}

func TestMaxTracesOTLP(t *testing.T) {
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	os.MkdirAll(inputDir, 0o755)

	marshaler := &ptrace.JSONMarshaler{}

	// Write 3 traces in OTLP format
	for i := 1; i <= 3; i++ {
		td := ptrace.NewTraces()
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", "svc")
		ss := rs.ScopeSpans().AppendEmpty()
		span := ss.Spans().AppendEmpty()
		traceHex := fmt.Sprintf("%032x", i)
		span.SetTraceID(hexToTraceID(traceHex))
		span.SetSpanID(hexToSpanID(fmt.Sprintf("%016x", i)))
		span.SetName(fmt.Sprintf("op-%d", i))
		span.SetStartTimestamp(pcommon.Timestamp(int64(i) * 1_000_000_000_000))
		span.SetEndTimestamp(pcommon.Timestamp(int64(i)*1_000_000_000_000 + 1_000_000_000))

		data, _ := marshaler.MarshalTraces(td)
		os.WriteFile(filepath.Join(inputDir, fmt.Sprintf("trace%d.json", i)), data, 0o644)
	}

	outputDir := filepath.Join(tmpDir, "output")
	// maxTraces=2: should only include 2 of 3 traces
	if err := runTransform(inputDir, outputDir, nil, nil, 2, 0, false, 60_000_000); err != nil {
		t.Fatalf("runTransform failed: %v", err)
	}

	chunks, _ := filepath.Glob(filepath.Join(outputDir, "chunk_*.pb"))
	totalSpans := 0
	um := &ptrace.ProtoUnmarshaler{}
	for _, chunk := range chunks {
		data, _ := os.ReadFile(chunk)
		td, _ := um.UnmarshalTraces(data)
		totalSpans += countPdataSpans(td)
	}

	if totalSpans != 2 {
		t.Errorf("expected 2 spans (maxTraces=2), got %d", totalSpans)
	}
}

func TestRunAnalyze(t *testing.T) {
	// Create a JSONL directory with test data
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	marshaler := &ptrace.JSONMarshaler{}

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "frontend")
	ss := rs.ScopeSpans().AppendEmpty()

	span := ss.Spans().AppendEmpty()
	span.SetTraceID(hexToTraceID("00000000000000000000000000000001"))
	span.SetSpanID(hexToSpanID("00000000000001a0"))
	span.SetName("GET /api")
	span.Attributes().PutStr("http.status_code", "200")
	span.Attributes().PutStr("http.url", "http://example.com/api")
	span.SetStartTimestamp(pcommon.Timestamp(1000000000000))
	span.SetEndTimestamp(pcommon.Timestamp(1000005000000))

	data, _ := marshaler.MarshalTraces(td)
	if err := os.WriteFile(filepath.Join(inputDir, "test.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// runDatasetStats should succeed
	if err := runDatasetStats(inputDir, 1, "", []string{"service.name", "span.kind", "operation.name", "status.code"}, "all"); err != nil {
		t.Fatalf("runDatasetStats failed: %v", err)
	}
}

// TestTransformKeepsTracesIntact verifies that maxSpansPerChunk never splits a
// single trace's spans across multiple output sub-files. Without this property,
// downstream per-file trace reconstruction produces fragments that break
// template-mode training and dataset-stats template counts.
func TestTransformKeepsTracesIntact(t *testing.T) {
	tmpDir := t.TempDir()
	inputDir := filepath.Join(tmpDir, "input")
	outputDir := filepath.Join(tmpDir, "output")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const numTraces = 10
	const spansPerTrace = 200
	const maxSpansPerChunk = 300 // forces multiple sub-files per bucket

	marshaler := &ptrace.JSONMarshaler{}
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	ss := rs.ScopeSpans().AppendEmpty()
	for ti := range numTraces {
		tidHex := fmt.Sprintf("%032x", ti+1)
		for si := range spansPerTrace {
			sp := ss.Spans().AppendEmpty()
			sp.SetTraceID(hexToTraceID(tidHex))
			sp.SetSpanID(hexToSpanID(fmt.Sprintf("%016x", ti*10000+si+1)))
			sp.SetName("op")
			sp.SetStartTimestamp(pcommon.Timestamp(1_000_000_000_000 + int64(si)*1_000_000))
			sp.SetEndTimestamp(pcommon.Timestamp(1_000_000_000_000 + int64(si)*1_000_000 + 500_000))
		}
	}
	data, _ := marshaler.MarshalTraces(td)
	if err := os.WriteFile(filepath.Join(inputDir, "in.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runTransform(inputDir, outputDir, nil, nil, 0, maxSpansPerChunk, false, 60_000_000); err != nil {
		t.Fatalf("runTransform failed: %v", err)
	}

	// Read every output chunk; assert each traceID lives in exactly one file
	// and that total spans are preserved.
	pbFiles, err := filepath.Glob(filepath.Join(outputDir, "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pbFiles) < 2 {
		t.Fatalf("expected multiple sub-files with maxSpansPerChunk=%d on %d spans, got %d", maxSpansPerChunk, numTraces*spansPerTrace, len(pbFiles))
	}

	traceFiles := make(map[pcommon.TraceID]map[string]int) // traceID -> file -> span count
	totalSpans := 0
	unmarshaler := &ptrace.ProtoUnmarshaler{}
	for _, f := range pbFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		outTd, err := unmarshaler.UnmarshalTraces(raw)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", f, err)
		}
		for i := 0; i < outTd.ResourceSpans().Len(); i++ {
			ors := outTd.ResourceSpans().At(i)
			for j := 0; j < ors.ScopeSpans().Len(); j++ {
				oss := ors.ScopeSpans().At(j)
				for k := 0; k < oss.Spans().Len(); k++ {
					span := oss.Spans().At(k)
					tid := span.TraceID()
					if traceFiles[tid] == nil {
						traceFiles[tid] = make(map[string]int)
					}
					traceFiles[tid][f]++
					totalSpans++
				}
			}
		}
	}

	if totalSpans != numTraces*spansPerTrace {
		t.Errorf("span count preserved: got %d, want %d", totalSpans, numTraces*spansPerTrace)
	}
	if len(traceFiles) != numTraces {
		t.Errorf("trace count: got %d, want %d", len(traceFiles), numTraces)
	}
	for tid, files := range traceFiles {
		if len(files) != 1 {
			t.Errorf("trace %s split across %d files: %v", hex.EncodeToString(tid[:]), len(files), files)
		}
	}
}
