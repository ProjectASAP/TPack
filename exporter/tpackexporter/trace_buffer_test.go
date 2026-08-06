package tpackexporter

import (
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

func makeTrace(traceID string, spans map[string]*tpackmodel.Span) *tpackmodel.Trace {
	return &tpackmodel.Trace{TraceID: traceID, Spans: spans}
}

func makeSpan(spanID, parentSpanID string, feature tpackmodel.SpanFeature) *tpackmodel.Span {
	return &tpackmodel.Span{
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		Feature:      feature,
		StartTime:    1000,
		Duration:     1000,
	}
}

func feat(svc, _, op, sc string) tpackmodel.SpanFeature {
	return tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{
		"service.name":   svc,
		"operation.name": op,
		"status.code":    sc,
	})
}

func TestTraceBufferAddAndSize(t *testing.T) {
	buf := newTraceBuffer(100)

	if buf.size() != 0 {
		t.Fatalf("expected size 0, got %d", buf.size())
	}

	buf.add([]*tpackmodel.Trace{
		makeTrace("trace-1", map[string]*tpackmodel.Span{
			"span-a": makeSpan("span-a", "", feat("svcA", "op1", "GET", "0")),
		}),
		makeTrace("trace-2", map[string]*tpackmodel.Span{
			"span-b": makeSpan("span-b", "", feat("svcB", "op2", "GET", "0")),
		}),
	})

	if buf.size() != 2 {
		t.Fatalf("expected size 2, got %d", buf.size())
	}
}

func TestTraceBufferMergesByTraceID(t *testing.T) {
	buf := newTraceBuffer(100)

	// First batch: service A's spans for trace-1
	buf.add([]*tpackmodel.Trace{
		makeTrace("trace-1", map[string]*tpackmodel.Span{
			"span-a": makeSpan("span-a", "", feat("svcA", "root", "GET", "0")),
		}),
	})

	// Second batch: service B's spans for the same trace-1
	buf.add([]*tpackmodel.Trace{
		makeTrace("trace-1", map[string]*tpackmodel.Span{
			"span-b": makeSpan("span-b", "span-a", feat("svcB", "child", "POST", "0")),
		}),
	})

	if buf.size() != 1 {
		t.Fatalf("expected 1 merged trace, got %d", buf.size())
	}

	traces := buf.flush()
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace after flush, got %d", len(traces))
	}

	tr := traces[0]
	if len(tr.Spans) != 2 {
		t.Fatalf("expected 2 spans in merged trace, got %d", len(tr.Spans))
	}
	if _, ok := tr.Spans["span-a"]; !ok {
		t.Error("missing span-a in merged trace")
	}
	if _, ok := tr.Spans["span-b"]; !ok {
		t.Error("missing span-b in merged trace")
	}
}

func TestTraceBufferMergeMultipleServices(t *testing.T) {
	buf := newTraceBuffer(100)

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("frontend", "ingress", "GET", "0")),
		}),
	})
	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s2": makeSpan("s2", "s1", feat("api", "handle", "POST", "0")),
		}),
	})
	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s3": makeSpan("s3", "s2", feat("db", "query", "SELECT", "0")),
		}),
	})

	if buf.size() != 1 {
		t.Fatalf("expected 1 trace, got %d", buf.size())
	}

	traces := buf.flush()
	tr := traces[0]
	if len(tr.Spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(tr.Spans))
	}

	child := tr.Spans["s3"]
	if _, ok := tr.Spans[child.ParentSpanID]; !ok {
		t.Error("cross-service parent link broken: s3's parent s2 not found in merged trace")
	}
}

func TestTraceBufferMergeAndDistinctTraces(t *testing.T) {
	buf := newTraceBuffer(100)

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svcA", "op", "GET", "0")),
		}),
		makeTrace("t2", map[string]*tpackmodel.Span{
			"s2": makeSpan("s2", "", feat("svcA", "op", "GET", "0")),
		}),
	})

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s3": makeSpan("s3", "s1", feat("svcB", "op", "POST", "0")),
		}),
	})

	if buf.size() != 2 {
		t.Fatalf("expected 2 distinct traces, got %d", buf.size())
	}

	traces := buf.flush()
	byID := map[string]*tpackmodel.Trace{}
	for _, tr := range traces {
		byID[tr.TraceID] = tr
	}

	if len(byID["t1"].Spans) != 2 {
		t.Errorf("t1: expected 2 spans, got %d", len(byID["t1"].Spans))
	}
	if len(byID["t2"].Spans) != 1 {
		t.Errorf("t2: expected 1 span, got %d", len(byID["t2"].Spans))
	}
}

func TestTraceBufferFlushClearsBuffer(t *testing.T) {
	buf := newTraceBuffer(100)

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svc", "op", "GET", "0")),
		}),
	})

	traces := buf.flush()
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace from flush, got %d", len(traces))
	}
	if buf.size() != 0 {
		t.Fatalf("expected size 0 after flush, got %d", buf.size())
	}

	traces = buf.flush()
	if len(traces) != 0 {
		t.Fatalf("expected 0 traces from second flush, got %d", len(traces))
	}
}

func TestTraceBufferFullSignal(t *testing.T) {
	buf := newTraceBuffer(2)

	full := buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svc", "op", "GET", "0")),
		}),
	})
	if full {
		t.Error("expected buffer not full with 1/2 traces")
	}

	full = buf.add([]*tpackmodel.Trace{
		makeTrace("t2", map[string]*tpackmodel.Span{
			"s2": makeSpan("s2", "", feat("svc", "op", "GET", "0")),
		}),
	})
	if !full {
		t.Error("expected buffer full at 2/2 traces")
	}
}

func TestTraceBufferFullNotTriggeredByMerge(t *testing.T) {
	buf := newTraceBuffer(2)

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svc", "op", "GET", "0")),
		}),
	})

	full := buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s2": makeSpan("s2", "s1", feat("svc", "op2", "POST", "0")),
		}),
	})
	if full {
		t.Error("merge into existing trace should not trigger full (still 1 trace)")
	}
	if buf.size() != 1 {
		t.Fatalf("expected size 1 after merge, got %d", buf.size())
	}
}

func TestTraceBufferDuplicateSpanOverwrite(t *testing.T) {
	buf := newTraceBuffer(100)

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svcA", "", "old_op", "0")),
		}),
	})

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svcA", "", "new_op", "0")),
		}),
	})

	traces := buf.flush()
	span := traces[0].Spans["s1"]
	if span.Feature.OperationName() != "new_op" {
		t.Errorf("expected overwritten operation name 'new_op', got %q", span.Feature.OperationName())
	}
}

func TestTraceBufferTimeSinceLastReceive(t *testing.T) {
	buf := newTraceBuffer(100)

	if buf.timeSinceLastReceive() != 0 {
		t.Error("expected 0 duration before any receives")
	}

	buf.add([]*tpackmodel.Trace{
		makeTrace("t1", map[string]*tpackmodel.Span{
			"s1": makeSpan("s1", "", feat("svc", "op", "GET", "0")),
		}),
	})

	d := buf.timeSinceLastReceive()
	if d < 0 {
		t.Errorf("expected non-negative duration, got %v", d)
	}
}
