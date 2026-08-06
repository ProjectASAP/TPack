package tpackmodel

import (
	"math"
	"math/rand"
	"testing"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

func TestRootDurationModelFitAndSample(t *testing.T) {
	config := DefaultConfig()
	m := NewRootDurationModel(config)

	f := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 2}

	// Create sample durations
	samples := map[NodeFeature][]float64{
		f: {100.0, 200.0, 150.0, 180.0, 120.0, 160.0, 140.0, 130.0, 170.0, 190.0},
	}

	m.FitFromSamples(samples)

	rng := rand.New(rand.NewSource(42))
	sum := 0.0
	n := 1000
	for range n {
		d := m.SampleDuration(f, rng, true)
		if d < 100.0 || d > 200.0 {
			t.Errorf("sample %f out of bounds [100, 200]", d)
		}
		sum += d
	}

	mean := sum / float64(n)
	if math.Abs(mean-155.0) > 20.0 {
		t.Errorf("expected sample mean ~155, got %f", mean)
	}
}

func TestRootDurationModelProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	m := NewRootDurationModel(config)

	f := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 3}
	samples := map[NodeFeature][]float64{
		f: {500.0, 600.0, 550.0, 580.0, 520.0},
	}
	m.FitFromSamples(samples)

	// Serialize
	models := &pb.TPackModels{}
	m.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	// Deserialize
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	m2 := NewRootDurationModel(config)
	m2.LoadStateDict(models2)

	// Verify we can sample from the loaded model
	rng := rand.New(rand.NewSource(42))
	d := m2.SampleDuration(f, rng, true)
	if d < 500.0 || d > 600.0 {
		t.Errorf("sample %f out of expected bounds [500, 600]", d)
	}
}
