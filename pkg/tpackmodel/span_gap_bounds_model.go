package tpackmodel

import (
	"math"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// SpanGapBoundsModel tracks min/max gap_from_parent bounds for all child spans
// keyed by NodeIdx.
type SpanGapBoundsModel struct {
	Config TPackConfig

	// bounds[NodeIdx] = {Min, Max}
	Bounds map[int32]MinMax
}

// NewSpanGapBoundsModel creates a new SpanGapBoundsModel.
func NewSpanGapBoundsModel(config TPackConfig) *SpanGapBoundsModel {
	return &SpanGapBoundsModel{
		Config: config,
		Bounds: make(map[int32]MinMax),
	}
}

// Update records a gap observation for the given nodeIdx.
func (m *SpanGapBoundsModel) Update(nodeIdx int32, gap float64) {
	if gap < 0 {
		return
	}
	bounds, ok := m.Bounds[nodeIdx]
	if !ok {
		bounds = MinMax{Min: math.Inf(1), Max: math.Inf(-1)}
	}
	if gap < bounds.Min {
		bounds.Min = gap
	}
	if gap > bounds.Max {
		bounds.Max = gap
	}
	m.Bounds[nodeIdx] = bounds
}

// GetGapBounds returns (min, max) gap for the given nodeIdx.
func (m *SpanGapBoundsModel) GetGapBounds(nodeIdx int32) (float64, float64) {
	if bounds, ok := m.Bounds[nodeIdx]; ok {
		return bounds.Min, bounds.Max
	}

	// Fallback: any bounds
	for _, bounds := range m.Bounds {
		return bounds.Min, bounds.Max
	}

	return 0.0, math.Inf(1)
}

// RemapNodeIdx translates all NodeIdx keys using the given mapping.
func (m *SpanGapBoundsModel) RemapNodeIdx(mapping []int32) {
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

// MergeFrom combines another SpanGapBoundsModel into this one.
func (m *SpanGapBoundsModel) MergeFrom(other *SpanGapBoundsModel) {
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

// SaveStateDict writes gap bounds into the protobuf message.
func (m *SpanGapBoundsModel) SaveStateDict(models *pb.TPackModels) {
	for nodeIdx, bounds := range m.Bounds {
		models.SpanGapBounds = append(models.SpanGapBounds, &pb.SpanGapBounds{
			Feature: &pb.NodeFeature{NodeIdx: nodeIdx},
			MinGap:  bounds.Min,
			MaxGap:  bounds.Max,
		})
	}
}

// LoadStateDict restores gap bounds from a protobuf message.
func (m *SpanGapBoundsModel) LoadStateDict(models *pb.TPackModels) {
	m.Bounds = make(map[int32]MinMax)

	for _, sgb := range models.SpanGapBounds {
		m.Bounds[sgb.Feature.NodeIdx] = MinMax{
			Min: sgb.MinGap,
			Max: sgb.MaxGap,
		}
	}
}
