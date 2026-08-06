package tpackmodel

import (
	"math"
	"math/rand"
	"runtime"
	"sync"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// durationModelEntry holds a GMM and its min/max duration bounds.
type durationModelEntry struct {
	GMM         *GaussianMixture1D
	MinDuration float64
	MaxDuration float64
}

// RootDurationModel models the duration of root spans using a GMM per
// NodeFeature. Durations are modeled in log-space: log(duration + 1).
type RootDurationModel struct {
	Config TPackConfig

	// models[NodeFeature] = entry
	Models map[NodeFeature]*durationModelEntry
}

// NewRootDurationModel creates a new RootDurationModel.
func NewRootDurationModel(config TPackConfig) *RootDurationModel {
	return &RootDurationModel{
		Config: config,
		Models: make(map[NodeFeature]*durationModelEntry),
	}
}

// FitFromSamples fits GMMs from collected log-duration samples.
// samples maps feature -> list of original durations.
// Each feature's GMM is fitted independently in parallel.
func (m *RootDurationModel) FitFromSamples(samples map[NodeFeature][]float64) {
	type result struct {
		feature NodeFeature
		entry   *durationModelEntry
	}

	results := make(chan result, len(samples))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup

	config := m.Config
	for feature, durations := range samples {
		if len(durations) == 0 {
			continue
		}
		wg.Add(1)
		go func(feature NodeFeature, durations []float64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Convert to log-space
			logDurations := make([]float64, len(durations))
			minDur := math.Inf(1)
			maxDur := math.Inf(-1)
			for i, d := range durations {
				logDurations[i] = math.Log(d + 1)
				if d < minDur {
					minDur = d
				}
				if d > maxDur {
					maxDur = d
				}
			}

			nComponents := int(config.MaxGMMComponents)
			maxByData := max(1, len(logDurations)/10)
			if nComponents > maxByData {
				nComponents = maxByData
			}

			var gmm *GaussianMixture1D
			if len(logDurations) >= int(config.MinSamplesForGMM) {
				gmm = NewGaussianMixture1D(nComponents)
				gmm.Fit(logDurations)
			} else if len(logDurations) == 1 {
				gmm = NewGaussianMixture1D(1)
				gmm.Weights[0] = 1.0
				gmm.Means[0] = logDurations[0]
				gmm.Variances[0] = 1e-6
			} else {
				return
			}

			results <- result{feature, &durationModelEntry{
				GMM:         gmm,
				MinDuration: minDur,
				MaxDuration: maxDur,
			}}
		}(feature, durations)
	}

	go func() { wg.Wait(); close(results) }()

	for r := range results {
		m.Models[r.feature] = r.entry
	}
}

// SampleDuration samples a duration for the given feature.
// If useBounds is true, uses reject sampling with bounds checking (matching the
// Python implementation); otherwise samples directly from the GMM without any
// observed-range clamp.
func (m *RootDurationModel) SampleDuration(feature NodeFeature, rng *rand.Rand, useBounds bool) float64 {
	entry := m.getModel(feature)
	if entry == nil {
		panic("RootDurationModel.SampleDuration: model has no entries (was FitFromSamples called?)")
	}

	if !useBounds {
		logDur := entry.GMM.Sample(rng)
		dur := math.Exp(logDur) - 1
		return math.Max(1.0, dur)
	}

	if entry.MinDuration == entry.MaxDuration {
		return entry.MinDuration
	}

	// Reject sampling
	const maxAttempts = 100
	for range maxAttempts {
		logDur := entry.GMM.Sample(rng)
		dur := math.Exp(logDur) - 1 // Reverse log(duration + 1)

		if dur >= entry.MinDuration && dur <= entry.MaxDuration {
			return math.Max(1.0, dur)
		}
	}

	// Fallback: midpoint
	return (entry.MinDuration + entry.MaxDuration) / 2
}

// getModel returns the model entry for feature, using fallbacks.
func (m *RootDurationModel) getModel(feature NodeFeature) *durationModelEntry {
	// Direct lookup
	if entry, ok := m.Models[feature]; ok {
		return entry
	}

	// Fallback 1: same node_idx with any child count
	for f, entry := range m.Models {
		if f.NodeIdx == feature.NodeIdx {
			return entry
		}
	}

	// Fallback 2: any model
	for _, entry := range m.Models {
		return entry
	}

	return nil
}

// SaveStateDict writes the root duration model into the protobuf message.
func (m *RootDurationModel) SaveStateDict(models *pb.TPackModels) {
	for feature, entry := range m.Models {
		gmm := entry.GMM
		rdm := &pb.RootDurationModel{
			Feature:     feature.ToProto(),
			MinDuration: entry.MinDuration,
			MaxDuration: entry.MaxDuration,
			Distribution: &pb.GaussianMixtureParams{
				NComponents: int32(gmm.Components),
				Weights:     gmm.Weights,
				Means:       gmm.Means,
				Variances:   gmm.Variances,
			},
		}
		models.RootDurationModels = append(models.RootDurationModels, rdm)
	}
}

// LoadStateDict restores the root duration model from a protobuf message.
func (m *RootDurationModel) LoadStateDict(models *pb.TPackModels) {
	m.Models = make(map[NodeFeature]*durationModelEntry)

	for _, rdm := range models.RootDurationModels {
		feature := NodeFeatureFromProto(rdm.Feature)

		nComp := int(rdm.Distribution.NComponents)
		gmm := NewGaussianMixture1D(nComp)
		gmm.Weights = make([]float64, nComp)
		gmm.Means = make([]float64, nComp)
		gmm.Variances = make([]float64, nComp)
		copy(gmm.Weights, rdm.Distribution.Weights)
		copy(gmm.Means, rdm.Distribution.Means)
		copy(gmm.Variances, rdm.Distribution.Variances)

		m.Models[feature] = &durationModelEntry{
			GMM:         gmm,
			MinDuration: rdm.MinDuration,
			MaxDuration: rdm.MaxDuration,
		}
	}
}
