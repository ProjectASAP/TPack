package tpackmodel

import (
	"math/rand"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// MetadataSampleRequest represents a single request for metadata prediction.
type MetadataSampleRequest struct {
	ParentFeature      NodeFeature
	ChildFeature       NodeFeature
	ParentDuration     float64
	NormalizedChildIdx float64 // child_idx / child_count
}

// MetadataSampleResult holds the output of a metadata prediction.
type MetadataSampleResult struct {
	GapRatio        float64 // [0, 1]
	DurationRatio   float64 // [0, 1]
	MetadataIndices []int   // Index per metadata column into its vocabulary
}

// DependentAttributePredictor is the interface for metadata (gap, duration, status) prediction.
// StatisticalDependentAttributePredictor: empirical distributions (no ML).
type DependentAttributePredictor interface {
	// SampleBatch generates metadata for a batch of requests.
	// rng must be provided for thread-safe concurrent generation.
	SampleBatch(
		requests []MetadataSampleRequest,
		durationBounds []MinMax,
		gapBounds []MinMax,
		rng *rand.Rand,
	) []MetadataSampleResult

	// SaveStateDict writes predictor state to protobuf.
	SaveStateDict(models *pb.TPackModels)
}
