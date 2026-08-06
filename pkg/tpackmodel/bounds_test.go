package tpackmodel

import (
	"math"
	"testing"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

func TestSpanDurationBoundsBasic(t *testing.T) {
	config := DefaultConfig()
	m := NewSpanDurationBoundsModel(config)

	var nodeIdx int32 = 0

	m.Update(nodeIdx, 10.0)
	m.Update(nodeIdx, 50.0)
	m.Update(nodeIdx, 30.0)

	minD, maxD := m.GetDurationBounds(nodeIdx)
	if minD != 10.0 {
		t.Errorf("expected min 10.0, got %f", minD)
	}
	if maxD != 50.0 {
		t.Errorf("expected max 50.0, got %f", maxD)
	}
}

func TestSpanDurationBoundsProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	m := NewSpanDurationBoundsModel(config)

	var nodeIdx int32 = 5
	m.Update(nodeIdx, 5.0)
	m.Update(nodeIdx, 100.0)

	models := &pb.TPackModels{}
	m.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	m2 := NewSpanDurationBoundsModel(config)
	m2.LoadStateDict(models2)

	minD, maxD := m2.GetDurationBounds(nodeIdx)
	if minD != 5.0 || maxD != 100.0 {
		t.Errorf("expected (5, 100), got (%f, %f)", minD, maxD)
	}
}

func TestSpanGapBoundsBasic(t *testing.T) {
	config := DefaultConfig()
	m := NewSpanGapBoundsModel(config)

	var nodeIdx int32 = 0

	m.Update(nodeIdx, 0.0)
	m.Update(nodeIdx, 1000.0)
	m.Update(nodeIdx, 500.0)

	minG, maxG := m.GetGapBounds(nodeIdx)
	if minG != 0.0 {
		t.Errorf("expected min 0.0, got %f", minG)
	}
	if maxG != 1000.0 {
		t.Errorf("expected max 1000.0, got %f", maxG)
	}
}

func TestSpanGapBoundsDefaultFallback(t *testing.T) {
	config := DefaultConfig()
	m := NewSpanGapBoundsModel(config)

	var nodeIdx int32 = 99
	minG, maxG := m.GetGapBounds(nodeIdx)

	if minG != 0.0 {
		t.Errorf("expected default min 0.0, got %f", minG)
	}
	if !math.IsInf(maxG, 1) {
		t.Errorf("expected default max +Inf, got %f", maxG)
	}
}

func TestSpanGapBoundsProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	m := NewSpanGapBoundsModel(config)

	var nodeIdx int32 = 3
	m.Update(nodeIdx, 10.0)
	m.Update(nodeIdx, 200.0)

	models := &pb.TPackModels{}
	m.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	m2 := NewSpanGapBoundsModel(config)
	m2.LoadStateDict(models2)

	minG, maxG := m2.GetGapBounds(nodeIdx)
	if minG != 10.0 || maxG != 200.0 {
		t.Errorf("expected (10, 200), got (%f, %f)", minG, maxG)
	}
}
