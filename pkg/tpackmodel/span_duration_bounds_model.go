package tpackmodel

import (
	"math"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// SpanDurationBoundsModel tracks min/max duration bounds for all spans
// keyed by NodeIdx.
type SpanDurationBoundsModel struct {
	Config TPackConfig

	// bounds[NodeIdx] = {Min, Max}
	Bounds map[int32]MinMax
}

// MinMax holds a min/max pair.
type MinMax struct {
	Min float64
	Max float64
}

// NewSpanDurationBoundsModel creates a new SpanDurationBoundsModel.
func NewSpanDurationBoundsModel(config TPackConfig) *SpanDurationBoundsModel {
	return &SpanDurationBoundsModel{
		Config: config,
		Bounds: make(map[int32]MinMax),
	}
}

// Update records a duration observation for the given nodeIdx.
func (m *SpanDurationBoundsModel) Update(nodeIdx int32, duration float64) {
	if duration <= 0 {
		return
	}
	bounds, ok := m.Bounds[nodeIdx]
	if !ok {
		bounds = MinMax{Min: math.Inf(1), Max: math.Inf(-1)}
	}
	if duration < bounds.Min {
		bounds.Min = duration
	}
	if duration > bounds.Max {
		bounds.Max = duration
	}
	m.Bounds[nodeIdx] = bounds
}

// GetDurationBounds returns (min, max) duration for the given nodeIdx.
func (m *SpanDurationBoundsModel) GetDurationBounds(nodeIdx int32) (float64, float64) {
	if bounds, ok := m.Bounds[nodeIdx]; ok {
		return bounds.Min, bounds.Max
	}

	// Fallback: any bounds
	for _, bounds := range m.Bounds {
		return bounds.Min, bounds.Max
	}

	return 1.0, math.Inf(1)
}

// RemapNodeIdx translates all NodeIdx keys using the given mapping.
func (m *SpanDurationBoundsModel) RemapNodeIdx(mapping []int32) {
	remapped := make(map[int32]MinMax, len(m.Bounds))
	for idx, b := range m.Bounds {
		newIdx := idx
		if int(idx) < len(mapping) {
			newIdx = mapping[idx]
		}
		if existing, ok := remapped[newIdx]; ok {
			if b.Min < existing.Min {
				existing.Min = b.Min
			}
			if b.Max > existing.Max {
				existing.Max = b.Max
			}
			remapped[newIdx] = existing
		} else {
			remapped[newIdx] = b
		}
	}
	m.Bounds = remapped
}

// MergeFrom combines another SpanDurationBoundsModel into this one.
func (m *SpanDurationBoundsModel) MergeFrom(other *SpanDurationBoundsModel) {
	for idx, b := range other.Bounds {
		if existing, ok := m.Bounds[idx]; ok {
			if b.Min < existing.Min {
				existing.Min = b.Min
			}
			if b.Max > existing.Max {
				existing.Max = b.Max
			}
			m.Bounds[idx] = existing
		} else {
			m.Bounds[idx] = b
		}
	}
}

// SaveStateDict writes duration bounds into the protobuf message.
func (m *SpanDurationBoundsModel) SaveStateDict(models *pb.TPackModels) {
	for nodeIdx, bounds := range m.Bounds {
		models.SpanDurationBounds = append(models.SpanDurationBounds, &pb.SpanDurationBounds{
			Feature:     &pb.NodeFeature{NodeIdx: nodeIdx},
			MinDuration: bounds.Min,
			MaxDuration: bounds.Max,
		})
	}
}

// LoadStateDict restores duration bounds from a protobuf message.
func (m *SpanDurationBoundsModel) LoadStateDict(models *pb.TPackModels) {
	m.Bounds = make(map[int32]MinMax)

	for _, sdb := range models.SpanDurationBounds {
		m.Bounds[sdb.Feature.NodeIdx] = MinMax{
			Min: sdb.MinDuration,
			Max: sdb.MaxDuration,
		}
	}
}

