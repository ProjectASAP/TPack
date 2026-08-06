package tpackreceiver

import (
	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// convertAllTracesToBatch converts a chunk of generated traces (grouped by
// traceID) into a single ptrace.Traces batch suitable for downstream
// ConsumeTraces.
func convertAllTracesToBatch(
	allTraces map[string][]tpackmodel.GeneratedSpan,
	encoder *tpackmodel.NodeEncoder,
) ptrace.Traces {
	var spans []otlpconv.SpanData
	for traceID, generated := range allTraces {
		for _, gs := range generated {
			spans = append(spans, otlpconv.SpanData{
				TraceID:      traceID,
				SpanID:       gs.SpanID,
				ParentSpanID: gs.ParentSpanID,
				Feature:      encoder.InverseTransform(gs.NodeIdx),
				StartTime:    gs.StartTime,
				Duration:     gs.Duration,
				Metadata:     gs.Metadata,
			})
		}
	}

	td := ptrace.NewTraces()
	otlpconv.AppendSpans(td, spans)
	return td
}
