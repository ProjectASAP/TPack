package tpackreceiver

import (
	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// generateAllTraces is a thin wrapper around tpackmodel.GenerateBucket that
// returns generated spans grouped by traceID. Grouping is done here (rather
// than in tpackmodel) to preserve the receiver's per-trace gRPC chunking.
func generateAllTraces(state *tpackmodel.TPackModelState) (map[string][]tpackmodel.GeneratedSpan, int) {
	spans, totalSpans := tpackmodel.GenerateBucket(state, tpackmodel.GenerateOptions{})
	grouped := make(map[string][]tpackmodel.GeneratedSpan)
	for _, sp := range spans {
		grouped[sp.TraceID] = append(grouped[sp.TraceID], sp)
	}
	return grouped, totalSpans
}
