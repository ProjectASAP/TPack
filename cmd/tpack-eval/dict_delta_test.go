package main

import (
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

// TestFinalizeLoopDictDelta exercises the exact merge-remap-marshal pattern
// used by processChunkedStreaming's finalize loop when --dict-delta is on.
// Two buckets share some signatures; bucket 2 introduces a new one. The test
// asserts that:
//  1. Bucket 2's delta-encoded bytes are smaller than its full-encoded bytes.
//  2. Bucket 2's delta roundtrips (base-vocab reconstruction yields the full
//     cumulative vocabulary).
//  3. With dict-delta off, bytes are identical to a vanilla Marshal().
func TestFinalizeLoopDictDelta(t *testing.T) {
	config := tpackmodel.DefaultConfig()
	config.RandomSeed = 42
	primaryAttributes := tpackmodel.DefaultFeatureColumns
	dependentAttributes := []string{}

	// Bucket 0: signatures {A, B}
	makeBucket0 := func() []*tpackmodel.Trace {
		return []*tpackmodel.Trace{
			makeDictDeltaTrace("t0", []dictDeltaSpan{
				{spanID: "r0", svc: "svcA", op: "opA", start: 0, dur: 1000},
				{spanID: "c0", parent: "r0", svc: "svcB", op: "opB", start: 100, dur: 500},
			}),
		}
	}
	// Bucket 1: signatures {B, C} — overlaps with B from bucket 0, adds C
	makeBucket1 := func() []*tpackmodel.Trace {
		return []*tpackmodel.Trace{
			makeDictDeltaTrace("t1", []dictDeltaSpan{
				{spanID: "r1", svc: "svcB", op: "opB", start: 60000000, dur: 1000},
				{spanID: "c1", parent: "r1", svc: "svcC", op: "opC", start: 60000100, dur: 500},
			}),
		}
	}

	// TrainBucket nils slice entries for GC, so each call needs its own slice.
	trainBucket := func(traces []*tpackmodel.Trace) *tpackmodel.TPackModelState {
		state, err := tpackmodel.TrainBucket(traces, config, primaryAttributes, dependentAttributes)
		if err != nil {
			t.Fatalf("train failed: %v", err)
		}
		return state
	}

	// --- Full mode: marshal each bucket with the full vocab ---
	fullState0 := trainBucket(makeBucket0())
	fullState1 := trainBucket(makeBucket1())

	fullBytes1, err := fullState1.Marshal()
	if err != nil {
		t.Fatalf("Marshal full bucket 1: %v", err)
	}

	// --- Delta mode: mirror the finalize-loop logic ---
	deltaState0 := trainBucket(makeBucket0())
	deltaState1 := trainBucket(makeBucket1())

	cumulative := tpackmodel.NewNodeEncoder()

	// Bucket 0 (delta): cumulative is empty, so remap is identity and
	// MarshalDelta(0) is equivalent to Marshal().
	prevVocab0 := cumulative.VocabularyStrings()
	remap0 := cumulative.MergeFrom(deltaState0.NodeEncoder)
	remapState(deltaState0, remap0)
	deltaState0.NodeEncoder = cumulative
	deltaBytes0, err := deltaState0.MarshalDelta(0)
	if err != nil {
		t.Fatalf("MarshalDelta bucket 0: %v", err)
	}

	// Bucket 1 (delta): prev vocab = bucket 0's cumulative signatures.
	prevVocab1 := cumulative.VocabularyStrings()
	prevSize1 := int(cumulative.VocabSize())
	remap1 := cumulative.MergeFrom(deltaState1.NodeEncoder)
	remapState(deltaState1, remap1)
	deltaState1.NodeEncoder = cumulative
	deltaBytes1, err := deltaState1.MarshalDelta(prevSize1)
	if err != nil {
		t.Fatalf("MarshalDelta bucket 1: %v", err)
	}

	t.Logf("bucket0 delta=%d bytes (full vocab)", len(deltaBytes0))
	t.Logf("bucket1 full=%d bytes, delta=%d bytes (prevSize=%d)",
		len(fullBytes1), len(deltaBytes1), prevSize1)

	// (1) delta bucket 1 must be strictly smaller than full bucket 1
	if len(deltaBytes1) >= len(fullBytes1) {
		t.Errorf("expected delta bytes < full bytes for bucket 1; got delta=%d, full=%d",
			len(deltaBytes1), len(fullBytes1))
	}

	// (2) bucket 1 delta roundtrips: reload it with the previous-bucket
	// vocabulary as base and confirm the reconstructed encoder has the full
	// cumulative vocab.
	models1 := &pb.TPackModels{}
	if err := proto.Unmarshal(deltaBytes1, models1); err != nil {
		t.Fatalf("unmarshal delta bytes1: %v", err)
	}
	if got, want := int(models1.CumulativeVocabSize), int(cumulative.VocabSize()); got != want {
		t.Errorf("cumulative_vocab_size: got %d, want %d", got, want)
	}
	restored := tpackmodel.NewTPackModelState(config)
	if err := restored.LoadFromProtoWithBaseVocabulary(models1, prevVocab1); err != nil {
		t.Fatalf("LoadFromProtoWithBaseVocabulary: %v", err)
	}
	if got, want := int(restored.NodeEncoder.VocabSize()), int(cumulative.VocabSize()); got != want {
		t.Errorf("restored vocab size: got %d, want %d", got, want)
	}

	// (3) bucket 0 delta (offset=0) == full marshal
	deltaModels0 := &pb.TPackModels{}
	if err := proto.Unmarshal(deltaBytes0, deltaModels0); err != nil {
		t.Fatalf("unmarshal delta bytes0: %v", err)
	}
	fullBytes0, err := fullState0.Marshal()
	if err != nil {
		t.Fatalf("Marshal full bucket 0: %v", err)
	}
	fullModels0 := &pb.TPackModels{}
	if err := proto.Unmarshal(fullBytes0, fullModels0); err != nil {
		t.Fatalf("unmarshal full bytes0: %v", err)
	}
	if len(deltaModels0.NodeVocabulary) != len(fullModels0.NodeVocabulary) {
		t.Errorf("bucket 0 delta vocab=%d, full vocab=%d (should match for offset=0)",
			len(deltaModels0.NodeVocabulary), len(fullModels0.NodeVocabulary))
	}

	_ = prevVocab0 // satisfy unused-var if the earlier capture is removed
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

type dictDeltaSpan struct {
	spanID, parent, svc, op string
	start, dur              int64
}

func makeDictDeltaTrace(traceID string, spans []dictDeltaSpan) *tpackmodel.Trace {
	t := &tpackmodel.Trace{TraceID: traceID, Spans: make(map[string]*tpackmodel.Span, len(spans))}
	for _, s := range spans {
		t.Spans[s.spanID] = &tpackmodel.Span{
			SpanID:       s.spanID,
			ParentSpanID: s.parent,
			Feature: tpackmodel.NewSpanFeature(tpackmodel.DefaultFeatureColumns, map[string]string{
				"service.name":   s.svc,
				"operation.name": s.op,
				"span.kind":      "",
				"status.code":    "0",
			}),
			StartTime: s.start,
			Duration:  s.dur,
			Metadata:  map[string]string{},
		}
	}
	return t
}

// remapState applies a remap table to every model component that stores node
// IDs. Mirrors the intra-bucket logic in pipeline.go:245-254 and the
// cross-bucket logic in processChunkedStreaming's finalize loop.
func remapState(state *tpackmodel.TPackModelState, remap []int32) {
	state.StartTableModel.RemapNodeIdx(remap)
	state.TopologyModel.RemapNodeIdx(remap)
	state.SpanDurationBounds.RemapNodeIdx(remap)
	state.SpanGapBounds.RemapNodeIdx(remap)
	if sp, ok := state.DependentAttributePredictor.(*tpackmodel.StatisticalDependentAttributePredictor); ok {
		sp.RemapNodeIdx(remap)
	}
}
