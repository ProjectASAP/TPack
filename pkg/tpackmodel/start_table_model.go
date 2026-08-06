package tpackmodel

import (
	"math/rand"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// StartTableModel tracks exact counts of root node features per trace_type.
// During generation, it replays these exact counts to faithfully reproduce the original distribution.
type StartTableModel struct {
	Config TPackConfig

	// root_counts[TraceType][NodeFeature] = count
	RootCounts  map[TraceType]map[NodeFeature]int32
	TotalCounts map[TraceType]int32
}

// NewStartTableModel creates a new StartTableModel.
func NewStartTableModel(config TPackConfig) *StartTableModel {
	return &StartTableModel{
		Config: config,
		RootCounts: map[TraceType]map[NodeFeature]int32{
			TraceTypeNormal: {},
			TraceTypeError:  {},
		},
		TotalCounts: map[TraceType]int32{
			TraceTypeNormal: 0,
			TraceTypeError:  0,
		},
	}
}

// AddRoot records a root node observation.
func (rm *StartTableModel) AddRoot(traceType TraceType, feature NodeFeature) {
	rm.RootCounts[traceType][feature]++
	rm.TotalCounts[traceType]++
}

// RootSample represents a single root feature with its context.
type RootSample struct {
	Feature   NodeFeature
	TraceType TraceType
	Template  *TraceTemplate // non-nil in template topology mode
}

// SampleRootFeaturesStratified returns all root features with exact counts,
// shuffled randomly. Each (feature, trace_type) entry is repeated
// according to its count.
func (rm *StartTableModel) SampleRootFeaturesStratified(rng *rand.Rand) []RootSample {
	var results []RootSample

	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		for feature, count := range rm.RootCounts[traceType] {
			for range count {
				results = append(results, RootSample{
					Feature:   feature,
					TraceType: traceType,
				})
			}
		}
	}

	// Shuffle
	rng.Shuffle(len(results), func(i, j int) {
		results[i], results[j] = results[j], results[i]
	})

	return results
}

// RemapNodeIdx translates all NodeIdx references using the given mapping.
func (rm *StartTableModel) RemapNodeIdx(mapping []int32) {
	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		remapped := make(map[NodeFeature]int32, len(rm.RootCounts[traceType]))
		for f, count := range rm.RootCounts[traceType] {
			remapped[RemapNodeFeature(f, mapping)] += count
		}
		rm.RootCounts[traceType] = remapped
	}
}

// MergeFrom combines another StartTableModel's counts into this one.
// Both must use the same NodeIdx encoding (call RemapNodeIdx on other first).
func (rm *StartTableModel) MergeFrom(other *StartTableModel) {
	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		for f, count := range other.RootCounts[traceType] {
			rm.RootCounts[traceType][f] += count
		}
		rm.TotalCounts[traceType] += other.TotalCounts[traceType]
	}
}

// SaveStateDict writes the root model state into the protobuf message.
func (rm *StartTableModel) SaveStateDict(models *pb.TPackModels) {
	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		protoTT := traceType.ToProto()

		for feature, count := range rm.RootCounts[traceType] {
			models.RootModels = append(models.RootModels, &pb.RootCountModel{
				Feature:   feature.ToProto(),
				Count:     count,
				TraceType: protoTT,
			})
		}
	}
}

// LoadStateDict restores the root model state from a protobuf message.
func (rm *StartTableModel) LoadStateDict(models *pb.TPackModels) {
	rm.RootCounts = map[TraceType]map[NodeFeature]int32{
		TraceTypeNormal: {},
		TraceTypeError:  {},
	}
	rm.TotalCounts = map[TraceType]int32{
		TraceTypeNormal: 0,
		TraceTypeError:  0,
	}

	for _, rcm := range models.RootModels {
		feature := NodeFeatureFromProto(rcm.Feature)
		traceType := TraceTypeFromProto(rcm.TraceType)

		rm.RootCounts[traceType][feature] = rcm.Count
		rm.TotalCounts[traceType] += rcm.Count
	}
}
