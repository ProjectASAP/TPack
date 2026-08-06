package tpackreceiver

import (
	"math/rand"
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// buildTestModelState creates a minimal but complete TPackModelState for testing.
// Models a 2-service topology: frontend(root, 2 children) → api + cache.
func buildTestModelState() *tpackmodel.TPackModelState {
	config := tpackmodel.DefaultConfig()
	config.StratifiedSampling = false
	state := tpackmodel.NewTPackModelState(config)

	// Fit encoder with 3 services
	fc := tpackmodel.DefaultFeatureColumns
	state.NodeEncoder.Fit([]tpackmodel.SpanFeature{
		tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "frontend", "operation.name": "GET", "status.code": "0"}),
		tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "api", "operation.name": "POST", "status.code": "0"}),
		tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "cache", "operation.name": "GET", "status.code": "0"}),
	})

	frontendIdx := state.NodeEncoder.Transform(tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "frontend", "operation.name": "GET", "status.code": "0"}))
	apiIdx := state.NodeEncoder.Transform(tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "api", "operation.name": "POST", "status.code": "0"}))
	cacheIdx := state.NodeEncoder.Transform(tpackmodel.NewSpanFeature(fc, map[string]string{"service.name": "cache", "operation.name": "GET", "status.code": "0"}))

	// Root feature: frontend with 2 children
	rootFeature := tpackmodel.NodeFeature{NodeIdx: frontendIdx, ChildIdx: 0, ChildCount: 2}

	// Add 5 root observations (simulates 5 traces)
	for range 5 {
		state.StartTableModel.AddRoot(tpackmodel.TraceTypeNormal, rootFeature)
	}

	// Parent reference feature for frontend (ChildIdx=-1 per convention)
	parentRef := tpackmodel.NodeFeature{NodeIdx: frontendIdx, ChildIdx: -1, ChildCount: 2}

	// Child features
	apiChild := tpackmodel.NodeFeature{NodeIdx: apiIdx, ChildIdx: 0, ChildCount: 0}
	cacheChild := tpackmodel.NodeFeature{NodeIdx: cacheIdx, ChildIdx: 1, ChildCount: 0}

	// Add edges: frontend → api at position 0, frontend → cache at position 1
	for range 5 {
		state.TopologyModel.AddEdge(tpackmodel.TraceTypeNormal, parentRef, 0, apiChild, 1)
		state.TopologyModel.AddEdge(tpackmodel.TraceTypeNormal, parentRef, 1, cacheChild, 1)
	}

	state.TopologyModel.MaxNodes = 3
	state.TopologyModel.BuildChildCandidatesCache()

	// Add root duration samples
	durationSamples := map[tpackmodel.NodeFeature][]float64{
		rootFeature: {5000, 6000, 5500, 4800, 5200},
	}
	state.RootDurationModel.FitFromSamples(durationSamples)

	// Add bounds
	state.SpanDurationBounds.Update(rootFeature.NodeIdx, 4800)
	state.SpanDurationBounds.Update(rootFeature.NodeIdx, 6000)
	state.SpanDurationBounds.Update(apiChild.NodeIdx, 1000)
	state.SpanDurationBounds.Update(apiChild.NodeIdx, 3000)
	state.SpanDurationBounds.Update(cacheChild.NodeIdx, 200)
	state.SpanDurationBounds.Update(cacheChild.NodeIdx, 800)

	state.SpanGapBounds.Update(apiChild.NodeIdx, 100)
	state.SpanGapBounds.Update(apiChild.NodeIdx, 500)
	state.SpanGapBounds.Update(cacheChild.NodeIdx, 50)
	state.SpanGapBounds.Update(cacheChild.NodeIdx, 300)

	// Train statistical metadata predictor (no metadata columns in this test).
	rng := rand.New(rand.NewSource(42))
	sp := tpackmodel.NewStatisticalDependentAttributePredictor(config, 0, rng)
	state.DependentAttributePredictor = sp
	for range 5 {
		sp.AddSample(frontendIdx, apiIdx, 0.1, 0.6, 5000, 0.0, nil)
		sp.AddSample(frontendIdx, cacheIdx, 0.2, 0.1, 5000, 1.0, nil)
	}
	sp.FinalizeFit()

	// Timing metadata
	state.MinStartTimeUs = 1000
	state.MaxStartTimeUs = 50000
	state.TraceCount = 5

	return state
}

func TestGenerateAllTracesDeterministic(t *testing.T) {
	state := buildTestModelState()

	// Generate traces twice with same seed — should produce identical span counts.
	traces1, total1 := generateAllTraces(state)
	traces2, total2 := generateAllTraces(state)

	if total1 != total2 {
		t.Errorf("expected deterministic span count, got %d vs %d", total1, total2)
	}
	if len(traces1) != len(traces2) {
		t.Errorf("expected deterministic trace count, got %d vs %d", len(traces1), len(traces2))
	}
}

func TestGenerateAllTracesSpanCount(t *testing.T) {
	state := buildTestModelState()

	allTraces, totalSpans := generateAllTraces(state)

	// We added 5 root observations, so should get 5 traces.
	if len(allTraces) != 5 {
		t.Errorf("expected 5 traces, got %d", len(allTraces))
	}

	// Each trace should have a root span (at minimum).
	for traceID, spans := range allTraces {
		if len(spans) == 0 {
			t.Errorf("trace %s has 0 spans", traceID)
		}

		hasRoot := false
		for _, span := range spans {
			if span.ParentSpanID == "" {
				hasRoot = true
				break
			}
		}
		if !hasRoot {
			t.Errorf("trace %s has no root span", traceID)
		}
	}

	// With MaxNodes=3 and topology (frontend→api+cache),
	// each trace should have exactly 3 spans.
	for _, spans := range allTraces {
		if len(spans) != 3 {
			t.Errorf("expected 3 spans per trace, got %d", len(spans))
		}
	}

	if totalSpans != 15 {
		t.Errorf("expected 15 total spans (5 traces * 3 spans), got %d", totalSpans)
	}
}

func TestGenerateAllTracesRootFeatures(t *testing.T) {
	state := buildTestModelState()

	allTraces, _ := generateAllTraces(state)

	frontendIdx := state.NodeEncoder.Transform(tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{"service.name": "frontend", "operation.name": "GET", "status.code": "0"}))

	for traceID, spans := range allTraces {
		for _, span := range spans {
			if span.ParentSpanID == "" {
				if span.NodeIdx != frontendIdx {
					t.Errorf("trace %s: root span has NodeIdx=%d, expected %d",
						traceID, span.NodeIdx, frontendIdx)
				}
			}
		}
	}
}

func TestGenerateAllTracesMaxNodesRespected(t *testing.T) {
	state := buildTestModelState()
	// Lower MaxNodes to 2 — should cap tree at root + 1 child.
	state.TopologyModel.MaxNodes = 2

	allTraces, _ := generateAllTraces(state)

	for traceID, spans := range allTraces {
		if len(spans) > 2 {
			t.Errorf("trace %s: expected at most 2 spans (MaxNodes=2), got %d",
				traceID, len(spans))
		}
	}
}

func TestGenerateAllTracesEmptyModel(t *testing.T) {
	config := tpackmodel.DefaultConfig()
	state := tpackmodel.NewTPackModelState(config)
	// No roots added, no topology — should produce no traces.

	allTraces, _ := generateAllTraces(state)

	if len(allTraces) != 0 {
		t.Errorf("expected 0 traces from empty model, got %d", len(allTraces))
	}
}
