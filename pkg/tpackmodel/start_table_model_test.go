package tpackmodel

import (
	"math/rand"
	"testing"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

func TestRootModelAddAndSample(t *testing.T) {
	config := DefaultConfig()
	rm := NewStartTableModel(config)

	f1 := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 3}
	f2 := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 2}

	// Add 5 normal roots with f1 and 3 error roots with f2
	for range 5 {
		rm.AddRoot(TraceTypeNormal, f1)
	}
	for range 3 {
		rm.AddRoot(TraceTypeError, f2)
	}

	rng := rand.New(rand.NewSource(42))
	samples := rm.SampleRootFeaturesStratified(rng)

	if len(samples) != 8 {
		t.Fatalf("expected 8 samples, got %d", len(samples))
	}

	normalCount := 0
	errorCount := 0
	for _, s := range samples {
		if s.TraceType == TraceTypeNormal {
			normalCount++
		} else {
			errorCount++
		}
	}
	if normalCount != 5 {
		t.Errorf("expected 5 normal, got %d", normalCount)
	}
	if errorCount != 3 {
		t.Errorf("expected 3 error, got %d", errorCount)
	}
}

func TestRootModelProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	rm := NewStartTableModel(config)

	f1 := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 3}
	f2 := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 2}

	rm.AddRoot(TraceTypeNormal, f1)
	rm.AddRoot(TraceTypeNormal, f1)
	rm.AddRoot(TraceTypeError, f2)

	// Serialize
	models := &pb.TPackModels{}
	rm.SaveStateDict(models)

	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	// Deserialize
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	rm2 := NewStartTableModel(config)
	rm2.LoadStateDict(models2)

	// Verify
	if rm2.RootCounts[TraceTypeNormal][f1] != 2 {
		t.Errorf("expected count 2, got %d", rm2.RootCounts[TraceTypeNormal][f1])
	}
	if rm2.RootCounts[TraceTypeError][f2] != 1 {
		t.Errorf("expected count 1, got %d", rm2.RootCounts[TraceTypeError][f2])
	}
}
