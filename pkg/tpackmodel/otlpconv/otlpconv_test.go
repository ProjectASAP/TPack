package otlpconv

import (
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// TestFromPdataMetadataIntAttribute regression-tests the bug where a
// non-string OTLP attribute (e.g. tracegen sets http.status_code via
// attribute.Int(...)) was being read with .Str() and silently became "".
// The receiver then dropped empty values, so the regenerated trace had no
// http.status_code attribute at all.
func TestFromPdataMetadataIntAttribute(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "frontend")
	ss := rs.ScopeSpans().AppendEmpty()

	span := ss.Spans().AppendEmpty()
	var tid pcommon.TraceID
	tid[15] = 1
	span.SetTraceID(tid)
	var sid pcommon.SpanID
	sid[7] = 1
	span.SetSpanID(sid)
	span.SetName("GET /api")
	span.SetStartTimestamp(pcommon.Timestamp(1000000000000))
	span.SetEndTimestamp(pcommon.Timestamp(1000005000000))
	span.Attributes().PutInt("http.status_code", 200)

	traces := FromPdata(td, []string{"service.name", "operation.name"}, []string{"http.status_code"})
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	var got string
	for _, sp := range traces[0].Spans {
		got = sp.Metadata["http.status_code"]
	}
	if got != "200" {
		t.Errorf(`metadata["http.status_code"] = %q, want "200" (was "" before .AsString fix)`, got)
	}
}

// TestRoundTripFromPtrace exercises the full path that runs in the docker
// demo: ptrace → FromPdata → TrainBucket → Marshal → Unmarshal →
// GenerateBucket → check Metadata is populated on regenerated spans.
// This catches issues that aren't visible from FromPdata alone (vocab
// serialization, predictor sampling, NO_PARENT root metadata).
func TestRoundTripFromPtrace(t *testing.T) {
	td := ptrace.NewTraces()
	for traceIdx := range 50 {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", "api-gateway")
		ss := rs.ScopeSpans().AppendEmpty()

		var tid pcommon.TraceID
		tid[14] = byte(traceIdx >> 8)
		tid[15] = byte(traceIdx)

		root := ss.Spans().AppendEmpty()
		root.SetTraceID(tid)
		var rid pcommon.SpanID
		rid[6] = byte(traceIdx >> 8)
		rid[7] = byte(traceIdx)
		root.SetSpanID(rid)
		root.SetName("GET /users")
		root.Status().SetCode(ptrace.StatusCodeUnset)
		root.SetStartTimestamp(pcommon.Timestamp(1_000_000_000_000 + int64(traceIdx)*1000))
		root.SetEndTimestamp(pcommon.Timestamp(1_000_000_005_000 + int64(traceIdx)*1000))
		root.Attributes().PutInt("http.status_code", 200)

		rs2 := td.ResourceSpans().AppendEmpty()
		rs2.Resource().Attributes().PutStr("service.name", "user-service")
		ss2 := rs2.ScopeSpans().AppendEmpty()
		child := ss2.Spans().AppendEmpty()
		child.SetTraceID(tid)
		var cid pcommon.SpanID
		cid[6] = byte(traceIdx >> 8)
		cid[7] = byte(traceIdx) + 1
		child.SetSpanID(cid)
		child.SetParentSpanID(rid)
		child.SetName("getUser")
		child.SetStartTimestamp(pcommon.Timestamp(1_000_000_001_000 + int64(traceIdx)*1000))
		child.SetEndTimestamp(pcommon.Timestamp(1_000_000_004_000 + int64(traceIdx)*1000))
		child.Attributes().PutInt("http.status_code", 200)
	}

	featureCols := tpackmodel.DefaultFeatureColumns
	metaCols := []string{"http.status_code"}

	traces := FromPdata(td, featureCols, metaCols)
	if len(traces) == 0 {
		t.Fatalf("no traces converted")
	}
	for _, tr := range traces {
		for _, sp := range tr.Spans {
			if sp.Metadata["http.status_code"] != "200" {
				t.Fatalf("input metadata = %q, want 200", sp.Metadata["http.status_code"])
			}
		}
	}

	cfg := tpackmodel.DefaultConfig()
	state, err := tpackmodel.TrainBucket(traces, cfg, featureCols, metaCols)
	if err != nil {
		t.Fatalf("TrainBucket: %v", err)
	}
	if vals := state.DependentAttributeVocabs["http.status_code"]; len(vals) == 0 {
		t.Fatalf("DependentAttributeVocabs[http.status_code] is empty after training")
	}

	data, err := state.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	restored := tpackmodel.NewTPackModelState(tpackmodel.DefaultConfig())
	if err := restored.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if vals := restored.DependentAttributeVocabs["http.status_code"]; len(vals) == 0 {
		t.Fatalf("DependentAttributeVocabs[http.status_code] is empty after deserialization")
	}

	spans, _ := tpackmodel.GenerateBucket(restored, tpackmodel.GenerateOptions{BucketKey: 1})
	if len(spans) == 0 {
		t.Fatalf("no spans generated")
	}

	emptyCount := 0
	for _, sp := range spans {
		if sp.Metadata["http.status_code"] == "" {
			emptyCount++
		}
	}
	if emptyCount > 0 {
		t.Errorf("%d/%d generated spans have empty http.status_code", emptyCount, len(spans))
	}
}

// TestExtractFeatureValuesFallback verifies that span.kind / status.code
// fall back to span attributes when the OTLP fields are unset — this is
// what lets tpack-eval read its own regenerated traces correctly.
func TestExtractFeatureValuesFallback(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetName("op")
	// span.Kind() defaults to UNSPECIFIED, span.Status().Code() to UNSET.
	// Place values on attributes (the format ReconstructOTLP produces for
	// non-status/kind columns is .PutStr, but our regenerated traces use
	// the proper SetKind/SetCode — so this fallback is for compatibility
	// with externally-produced traces that store these as attributes).
	span.Attributes().PutStr("span.kind", "CLIENT")
	span.Attributes().PutStr("status.code", "ERROR")

	values := ExtractFeatureValues([]string{"span.kind", "status.code"}, "svc", span)
	if values["span.kind"] != "CLIENT" {
		t.Errorf("span.kind fallback = %q, want CLIENT", values["span.kind"])
	}
	if values["status.code"] != "ERROR" {
		t.Errorf("status.code fallback = %q, want ERROR", values["status.code"])
	}
}

// TestReconstructOTLPRoundTrip checks that ReconstructOTLP handles the
// well-known columns (kind, name, status.code) and writes everything else
// as a span attribute.
func TestReconstructOTLPRoundTrip(t *testing.T) {
	feat := tpackmodel.NewSpanFeature(
		[]string{"service.name", "span.kind", "operation.name", "status.code", "method.name"},
		map[string]string{
			"service.name":   "frontend",
			"span.kind":      "CLIENT",
			"operation.name": "GET /api",
			"status.code":    "2",
			"method.name":    "GET",
		},
	)

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()

	ReconstructOTLP(feat, span)

	if span.Name() != "GET /api" {
		t.Errorf("span.Name = %q, want GET /api", span.Name())
	}
	if span.Status().Code() != ptrace.StatusCodeError {
		t.Errorf("status.Code = %v, want Error", span.Status().Code())
	}
	if v, ok := span.Attributes().Get("method.name"); !ok || v.AsString() != "GET" {
		t.Errorf("method.name attribute missing or wrong: %v", v)
	}
}

// TestHexToBytes covers the hex-decoding helper used for trace/span IDs.
func TestHexToBytes(t *testing.T) {
	dst := make([]byte, 4)
	HexToBytes("deadbeef", dst)
	want := []byte{0xde, 0xad, 0xbe, 0xef}
	for i := range want {
		if dst[i] != want[i] {
			t.Errorf("byte %d = %#x, want %#x", i, dst[i], want[i])
		}
	}
}
