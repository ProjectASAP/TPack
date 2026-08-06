package tpackmodel

import (
	"math/rand"
	"testing"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

func TestTPackModelStateRoundtrip(t *testing.T) {
	config := DefaultConfig()
	state := NewTPackModelState(config)

	// Fit node encoder
	state.NodeEncoder.Fit([]SpanFeature{
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcA", "operation.name": "opA", "status.code": "0"}),
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcB", "operation.name": "opB", "status.code": "0"}),
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcC", "operation.name": "opC", "status.code": "2"}),
	})

	// Add some root data
	f1 := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 2}
	f2 := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 1}
	state.StartTableModel.AddRoot(TraceTypeNormal, f1)
	state.StartTableModel.AddRoot(TraceTypeNormal, f1)
	state.StartTableModel.AddRoot(TraceTypeError, f2)

	// Add topology edges
	parentFeature := NodeFeature{NodeIdx: 0, ChildIdx: -1, ChildCount: 2}
	child0 := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 0}
	state.TopologyModel.AddEdge(TraceTypeNormal, parentFeature, 0, child0, 5)
	state.TopologyModel.BuildChildCandidatesCache()
	state.TopologyModel.MaxNodes = 10

	// Add duration bounds
	state.SpanDurationBounds.Update(f1.NodeIdx, 100.0)
	state.SpanDurationBounds.Update(f1.NodeIdx, 500.0)

	// Add gap bounds
	state.SpanGapBounds.Update(child0.NodeIdx, 0.0)
	state.SpanGapBounds.Update(child0.NodeIdx, 50.0)

	// Add root duration samples
	durSamples := map[NodeFeature][]float64{
		f1: {100.0, 200.0, 150.0, 180.0, 120.0},
	}
	state.RootDurationModel.FitFromSamples(durSamples)

	// Serialize
	data, err := state.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Serialized model size: %d bytes", len(data))

	// Deserialize
	state2 := NewTPackModelState(DefaultConfig())
	if err := state2.Unmarshal(data); err != nil {
		t.Fatal(err)
	}

	// Verify node encoder
	if state2.NodeEncoder.VocabSize() != 3 {
		t.Errorf("expected vocab size 3, got %d", state2.NodeEncoder.VocabSize())
	}

	// Verify root model counts
	if state2.StartTableModel.RootCounts[TraceTypeNormal][f1] != 2 {
		t.Errorf("expected root count 2, got %d", state2.StartTableModel.RootCounts[TraceTypeNormal][f1])
	}

	// Verify topology model generates trees
	rng := rand.New(rand.NewSource(42))
	rootFeature := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 2}
	tree := state2.TopologyModel.GenerateTreeStructure(rootFeature, TraceTypeNormal, rng)
	if tree == nil {
		t.Error("expected tree generation to work after roundtrip")
	}

	// Verify duration bounds
	minD, maxD := state2.SpanDurationBounds.GetDurationBounds(f1.NodeIdx)
	if minD != 100.0 || maxD != 500.0 {
		t.Errorf("expected duration bounds (100, 500), got (%f, %f)", minD, maxD)
	}

	// Verify gap bounds
	minG, maxG := state2.SpanGapBounds.GetGapBounds(child0.NodeIdx)
	if minG != 0.0 || maxG != 50.0 {
		t.Errorf("expected gap bounds (0, 50), got (%f, %f)", minG, maxG)
	}
}

func TestTPackModelStateDeltaRoundtrip(t *testing.T) {
	config := DefaultConfig()

	// Simulate two buckets with overlapping + new features
	bucket1Features := []SpanFeature{
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcA", "operation.name": "opA", "status.code": "0"}),
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcB", "operation.name": "opB", "status.code": "0"}),
	}
	bucket2Features := []SpanFeature{
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcB", "operation.name": "opB", "status.code": "0"}), // overlap
		NewSpanFeature(DefaultFeatureColumns, map[string]string{"service.name": "svcC", "operation.name": "opC", "status.code": "2"}), // new
	}

	// Build cumulative encoder (simulating buildVocabPlan)
	encoder := NewNodeEncoder()
	offset1 := int(encoder.VocabSize()) // 0
	encoder.Extend(bucket1Features)
	offset2 := int(encoder.VocabSize()) // 2
	encoder.Extend(bucket2Features)
	totalVocab := int(encoder.VocabSize()) // 3

	if offset1 != 0 {
		t.Fatalf("expected offset1=0, got %d", offset1)
	}
	if offset2 != 2 {
		t.Fatalf("expected offset2=2, got %d", offset2)
	}
	if totalVocab != 3 {
		t.Fatalf("expected totalVocab=3, got %d", totalVocab)
	}

	// --- Bucket 1: train and serialize with delta ---
	state1 := NewTPackModelState(config)
	state1.NodeEncoder = encoder
	f1 := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 1}
	state1.StartTableModel.AddRoot(TraceTypeNormal, f1)
	state1.RootDurationModel.FitFromSamples(map[NodeFeature][]float64{f1: {100, 200}})

	delta1, err := state1.MarshalDelta(offset1) // offset=0, so all entries included
	if err != nil {
		t.Fatalf("MarshalDelta bucket1: %v", err)
	}

	// --- Bucket 2: train and serialize with delta ---
	state2 := NewTPackModelState(config)
	state2.NodeEncoder = encoder
	f2 := NodeFeature{NodeIdx: 2, ChildIdx: 0, ChildCount: 0}
	state2.StartTableModel.AddRoot(TraceTypeNormal, f2)
	state2.RootDurationModel.FitFromSamples(map[NodeFeature][]float64{f2: {300, 400}})

	delta2, err := state2.MarshalDelta(offset2) // offset=2, only 1 new entry
	if err != nil {
		t.Fatalf("MarshalDelta bucket2: %v", err)
	}

	t.Logf("Bucket1 delta size: %d bytes (full vocab), Bucket2 delta size: %d bytes (1 new entry)", len(delta1), len(delta2))

	// --- Verify delta2 proto has only 1 vocab entry ---
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(delta2, models2); err != nil {
		t.Fatalf("unmarshal delta2: %v", err)
	}
	if len(models2.NodeVocabulary) != 1 {
		t.Errorf("expected 1 delta vocab entry, got %d", len(models2.NodeVocabulary))
	}
	if models2.CumulativeVocabSize != int32(totalVocab) {
		t.Errorf("expected cumulative_vocab_size=%d, got %d", totalVocab, models2.CumulativeVocabSize)
	}

	// --- Roundtrip bucket 2 with base vocabulary ---
	baseVocab := encoder.VocabularyStrings()[:offset2]
	restored := NewTPackModelState(DefaultConfig())
	if err := restored.LoadFromProtoWithBaseVocabulary(models2, baseVocab); err != nil {
		t.Fatalf("LoadFromProtoWithBaseVocabulary: %v", err)
	}

	if restored.NodeEncoder.VocabSize() != int32(totalVocab) {
		t.Errorf("expected restored vocab size %d, got %d", totalVocab, restored.NodeEncoder.VocabSize())
	}

	// Verify all features can be inverse-transformed correctly
	for idx := int32(0); idx < int32(totalVocab); idx++ {
		orig := encoder.InverseTransform(idx)
		got := restored.NodeEncoder.InverseTransform(idx)
		if orig != got {
			t.Errorf("idx %d: expected %q, got %q", idx, orig, got)
		}
	}

	// Verify root model roundtripped
	if restored.StartTableModel.RootCounts[TraceTypeNormal][f2] != 1 {
		t.Errorf("expected root count 1 for f2, got %d", restored.StartTableModel.RootCounts[TraceTypeNormal][f2])
	}

	// --- Verify bucket 1 delta roundtrips without base vocab (offset=0 means all entries present) ---
	models1 := &pb.TPackModels{}
	if err := proto.Unmarshal(delta1, models1); err != nil {
		t.Fatalf("unmarshal delta1: %v", err)
	}
	if len(models1.NodeVocabulary) != totalVocab {
		t.Errorf("bucket1: expected %d vocab entries (full), got %d", totalVocab, len(models1.NodeVocabulary))
	}

	restored1 := NewTPackModelState(DefaultConfig())
	if err := restored1.LoadFromProtoWithBaseVocabulary(models1, nil); err != nil {
		t.Fatalf("LoadFromProtoWithBaseVocabulary bucket1: %v", err)
	}
	if restored1.NodeEncoder.VocabSize() != int32(totalVocab) {
		t.Errorf("bucket1 restored: expected vocab size %d, got %d", totalVocab, restored1.NodeEncoder.VocabSize())
	}

	// --- Verify legacy path (CumulativeVocabSize=0) ignores base vocab ---
	legacyData, err := state1.Marshal() // uses full Marshal, no delta
	if err != nil {
		t.Fatalf("Marshal legacy: %v", err)
	}
	legacyModels := &pb.TPackModels{}
	if err := proto.Unmarshal(legacyData, legacyModels); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacyModels.CumulativeVocabSize != 0 {
		t.Errorf("legacy should have CumulativeVocabSize=0, got %d", legacyModels.CumulativeVocabSize)
	}

	legacyRestored := NewTPackModelState(DefaultConfig())
	if err := legacyRestored.LoadFromProtoWithBaseVocabulary(legacyModels, baseVocab); err != nil {
		t.Fatalf("LoadFromProtoWithBaseVocabulary legacy: %v", err)
	}
	// Legacy path should have the full vocab from Marshal() (3 entries)
	if legacyRestored.NodeEncoder.VocabSize() != int32(totalVocab) {
		t.Errorf("legacy restored: expected vocab size %d, got %d", totalVocab, legacyRestored.NodeEncoder.VocabSize())
	}
}
