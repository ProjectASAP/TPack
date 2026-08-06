package tpackmodel

import (
	"testing"
)

// helper to build a SpanFeature with default columns
func testFeature(values map[string]string) SpanFeature {
	return NewSpanFeature(DefaultFeatureColumns, values)
}

func TestNodeEncoderFit(t *testing.T) {
	enc := NewNodeEncoder()
	enc.Fit([]SpanFeature{
		testFeature(map[string]string{"service.name": "svcA", "operation.name": "opA", "status.code": "0"}),
		testFeature(map[string]string{"service.name": "svcB", "operation.name": "opB", "status.code": "0"}),
		testFeature(map[string]string{"service.name": "svcA", "operation.name": "opA", "status.code": "0"}),
	})

	if !enc.IsFitted() {
		t.Fatal("expected encoder to be fitted")
	}
	if enc.VocabSize() != 2 {
		t.Fatalf("expected vocab size 2, got %d", enc.VocabSize())
	}
}

func TestNodeEncoderTransformRoundtrip(t *testing.T) {
	enc := NewNodeEncoder()
	features := []SpanFeature{
		testFeature(map[string]string{"service.name": "alpha"}),
		testFeature(map[string]string{"service.name": "beta"}),
		testFeature(map[string]string{"service.name": "gamma"}),
	}
	enc.Fit(features)

	for _, f := range features {
		idx := enc.Transform(f)
		got := enc.InverseTransform(idx)
		if got != f {
			t.Errorf("roundtrip failed: %v → %d → %v", f, idx, got)
		}
	}
}

func TestNodeEncoderUnknownMapsToZero(t *testing.T) {
	enc := NewNodeEncoder()
	enc.Fit([]SpanFeature{testFeature(map[string]string{"service.name": "known"})})

	idx := enc.Transform(testFeature(map[string]string{"service.name": "unknown"}))
	if idx != 0 {
		t.Errorf("expected unknown to map to 0, got %d", idx)
	}
}

func TestNodeEncoderFitFromVocabulary(t *testing.T) {
	enc := NewNodeEncoder()
	// Create canonical-format vocabulary strings
	f0 := testFeature(map[string]string{"service.name": "zero"})
	f1 := testFeature(map[string]string{"service.name": "one"})
	f2 := testFeature(map[string]string{"service.name": "two"})
	vocab := []string{string(f0), string(f1), string(f2)}
	enc.FitFromVocabulary(vocab)

	if enc.VocabSize() != 3 {
		t.Fatalf("expected vocab size 3, got %d", enc.VocabSize())
	}
	// FitFromVocabulary preserves order (no sorting)
	if enc.Transform(ParseSpanFeature(vocab[0])) != 0 {
		t.Error("expected first entry at index 0")
	}
	if enc.Transform(ParseSpanFeature(vocab[2])) != 2 {
		t.Error("expected third entry at index 2")
	}
}

func TestNodeEncoderDeterministicOrdering(t *testing.T) {
	enc1 := NewNodeEncoder()
	enc1.Fit([]SpanFeature{
		testFeature(map[string]string{"service.name": "c"}),
		testFeature(map[string]string{"service.name": "a"}),
		testFeature(map[string]string{"service.name": "b"}),
	})

	enc2 := NewNodeEncoder()
	enc2.Fit([]SpanFeature{
		testFeature(map[string]string{"service.name": "b"}),
		testFeature(map[string]string{"service.name": "c"}),
		testFeature(map[string]string{"service.name": "a"}),
	})

	// Both should produce the same mapping since Fit sorts
	for _, f := range []SpanFeature{
		testFeature(map[string]string{"service.name": "a"}),
		testFeature(map[string]string{"service.name": "b"}),
		testFeature(map[string]string{"service.name": "c"}),
	} {
		if enc1.Transform(f) != enc2.Transform(f) {
			t.Errorf("non-deterministic: %v maps to %d and %d",
				f, enc1.Transform(f), enc2.Transform(f))
		}
	}
}

func TestNodeEncoderVocabularyStrings(t *testing.T) {
	enc := NewNodeEncoder()
	f := testFeature(map[string]string{
		"service.name":   "svc",
		"span.kind":      "SERVER",
		"operation.name": "op",
		"status.code":    "0",
	})
	enc.Fit([]SpanFeature{f})

	strs := enc.VocabularyStrings()
	if len(strs) != 1 {
		t.Fatalf("expected 1 vocabulary string, got %d", len(strs))
	}

	// Round-trip through FitFromVocabulary
	enc2 := NewNodeEncoder()
	enc2.FitFromVocabulary(strs)
	feat := enc2.InverseTransform(0)
	if feat.SpanKind() != "SERVER" {
		t.Errorf("expected SpanKind=SERVER, got %q", feat.SpanKind())
	}
	if feat.ServiceName() != "svc" {
		t.Errorf("expected ServiceName=svc, got %q", feat.ServiceName())
	}
}

func TestSpanFeatureGetValues(t *testing.T) {
	f := NewSpanFeature([]string{"service.name", "span.kind", "operation.name"}, map[string]string{
		"service.name":   "frontend",
		"span.kind":      "SERVER",
		"operation.name": "GET /api",
	})

	if f.ServiceName() != "frontend" {
		t.Errorf("expected frontend, got %q", f.ServiceName())
	}
	if f.SpanKind() != "SERVER" {
		t.Errorf("expected SERVER, got %q", f.SpanKind())
	}
	if f.OperationName() != "GET /api" {
		t.Errorf("expected GET /api, got %q", f.OperationName())
	}
	if f.Get("missing") != "" {
		t.Errorf("expected empty for missing key")
	}

	vals := f.Values()
	if len(vals) != 3 {
		t.Errorf("expected 3 values, got %d", len(vals))
	}
}

func TestSpanFeatureIsError(t *testing.T) {
	errFeat := testFeature(map[string]string{"status.code": "2"})
	if !errFeat.IsError() {
		t.Error("expected IsError=true for status.code=2")
	}

	okFeat := testFeature(map[string]string{"status.code": "1"})
	if okFeat.IsError() {
		t.Error("expected IsError=false for status.code=1")
	}
}

func TestSpanFeatureCustomColumns(t *testing.T) {
	cols := []string{"service.name", "http.status_code"}
	f := NewSpanFeature(cols, map[string]string{
		"service.name":     "api",
		"http.status_code": "200",
	})

	if f.Get("service.name") != "api" {
		t.Errorf("expected api, got %q", f.Get("service.name"))
	}
	if f.Get("http.status_code") != "200" {
		t.Errorf("expected 200, got %q", f.Get("http.status_code"))
	}
}
