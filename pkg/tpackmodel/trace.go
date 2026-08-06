package tpackmodel

// Span is the internal representation of one span before training.
// It carries raw metadata strings; the training pipeline indexes them per
// metadata column.
type Span struct {
	SpanID       string
	ParentSpanID string // empty for root spans
	Feature      SpanFeature
	StartTime    int64             // microseconds
	Duration     int64             // microseconds
	Metadata     map[string]string // dynamic metadata columns (e.g. "http.status_code" → "200")
}

// Trace holds a converted trace ready for model training.
type Trace struct {
	TraceID string
	Spans   map[string]*Span // spanID → Span
}

// IsErrorTrace reports whether any span in the trace has an error status code.
func IsErrorTrace(t *Trace) bool {
	for _, s := range t.Spans {
		if s.Feature.IsError() {
			return true
		}
	}
	return false
}

// CollectFeatures returns the unique SpanFeatures across a slice of traces.
func CollectFeatures(traces []*Trace) []SpanFeature {
	seen := make(map[SpanFeature]struct{})
	for _, t := range traces {
		for _, s := range t.Spans {
			seen[s.Feature] = struct{}{}
		}
	}
	out := make([]SpanFeature, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	return out
}

// TraceFeatures returns the unique SpanFeatures within a single trace.
func TraceFeatures(t *Trace) []SpanFeature {
	seen := make(map[SpanFeature]struct{}, len(t.Spans))
	for _, s := range t.Spans {
		seen[s.Feature] = struct{}{}
	}
	out := make([]SpanFeature, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	return out
}
