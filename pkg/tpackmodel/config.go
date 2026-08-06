package tpackmodel

import pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"

// TPackConfig holds all configuration for the TPack algorithm.
type TPackConfig struct {
	// Topology model parameters
	MaxDepth    int32
	MaxChildren int32

	// GMM parameters for root duration
	MaxGMMComponents int32
	MinSamplesForGMM int32

	// Reject sampling parameters
	RejectSamplingMaxAttempts int32
	RejectSamplingEnabled     bool

	// Stratified sampling
	StratifiedSampling bool

	// Offset value space: "ratio" (gap/parentDur) or "absolute" (raw µs)
	OffsetValue string
	// Offset distribution model: "regression" (OLS) or "percentile" (empirical percentile interpolation)
	OffsetModel string

	UseDurationBounds bool   // Clamp generated durations/gaps to observed bounds (default: true)
	TopologyMode      string // "edge" (default) or "template" (full-tree memorization)

	// General
	RandomSeed int32
}

// DefaultConfig returns a TPackConfig with default values matching the Python defaults.
func DefaultConfig() TPackConfig {
	return TPackConfig{
		MaxDepth:                  100,
		MaxChildren:               5000,
		MaxGMMComponents:          3,
		MinSamplesForGMM:          2,
		RejectSamplingMaxAttempts: 10,
		RejectSamplingEnabled:     true,
		StratifiedSampling:        true,
		UseDurationBounds:         true,
		OffsetValue:               "ratio",
		OffsetModel:               "regression",
		TopologyMode:              "edge",
		RandomSeed:                42,
	}
}

// ToProto converts TPackConfig to its protobuf representation.
func (c TPackConfig) ToProto() *pb.TPackConfig {
	return &pb.TPackConfig{
		MaxDepth:                  c.MaxDepth,
		MaxChildren:               c.MaxChildren,
		MaxGmmComponents:          c.MaxGMMComponents,
		MinSamplesForGmm:          c.MinSamplesForGMM,
		RejectSamplingMaxAttempts: c.RejectSamplingMaxAttempts,
		RejectSamplingEnabled:     c.RejectSamplingEnabled,
		StratifiedSampling:        c.StratifiedSampling,
		UseDurationBounds:         c.UseDurationBounds,
		TopologyMode:              c.TopologyMode,
		RandomSeed:                c.RandomSeed,
		OffsetValue:               c.OffsetValue,
		OffsetModel:               c.OffsetModel,
	}
}

// ConfigFromProto converts a protobuf TPackConfig to the Go type.
func ConfigFromProto(p *pb.TPackConfig) TPackConfig {
	if p == nil {
		return DefaultConfig()
	}
	return TPackConfig{
		MaxDepth:                  p.MaxDepth,
		MaxChildren:               p.MaxChildren,
		MaxGMMComponents:          p.MaxGmmComponents,
		MinSamplesForGMM:          p.MinSamplesForGmm,
		RejectSamplingMaxAttempts: p.RejectSamplingMaxAttempts,
		RejectSamplingEnabled:     p.RejectSamplingEnabled,
		StratifiedSampling:        p.StratifiedSampling,
		UseDurationBounds:         p.UseDurationBounds,
		TopologyMode:              p.TopologyMode,
		RandomSeed:                p.RandomSeed,
		OffsetValue:               p.OffsetValue,
		OffsetModel:               p.OffsetModel,
	}
}
