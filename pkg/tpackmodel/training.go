package tpackmodel

import (
	"log"
	"math"
	"math/rand"
	"sync"
	"time"
)

// StreamingTrainer accumulates model statistics one trace at a time and
// finalizes to a complete TPackModelState.
//
// Usage:
//
//	st := NewStreamingTrainer(cfg, featureCols, metadataCols)
//	for _, t := range traces { st.AddTrace(t) }
//	state := st.Finalize()
//
// Multiple trainers can be combined with MergeTrainers before Finalize.
type StreamingTrainer struct {
	state           *TPackModelState
	dependentAttributes []string
	vocabIdx        map[string]map[string]int // column → value → index (mirrors state.DependentAttributeVocabs ordering)
	durationSamples map[NodeFeature][]float64
	globalMinStart  int64
	globalMaxStart  int64
	maxTreeSize     int32
	traceCount      int32
}

// NewStreamingTrainer creates a fresh trainer with the given config and column sets.
func NewStreamingTrainer(cfg TPackConfig, primaryAttributes, dependentAttributes []string) *StreamingTrainer {
	state := NewTPackModelState(cfg)
	state.PrimaryAttributes = primaryAttributes
	state.DependentAttributes = dependentAttributes

	vocabIdx := make(map[string]map[string]int, len(dependentAttributes))
	for _, col := range dependentAttributes {
		vocabIdx[col] = make(map[string]int)
	}

	rng := rand.New(rand.NewSource(int64(cfg.RandomSeed)))
	state.DependentAttributePredictor = NewStatisticalDependentAttributePredictor(cfg, len(dependentAttributes), rng)

	return &StreamingTrainer{
		state:           state,
		dependentAttributes: dependentAttributes,
		vocabIdx:        vocabIdx,
		durationSamples: make(map[NodeFeature][]float64),
		globalMinStart:  math.MaxInt64,
		globalMaxStart:  math.MinInt64,
	}
}

// State returns the underlying state. During training (before Finalize) the
// fields grow with each AddTrace; only after Finalize are GMM/cache/predictor
// fully built.
func (st *StreamingTrainer) State() *TPackModelState { return st.state }

// AddTrace indexes a trace and trains all per-trace models from it.
// Safe to call repeatedly; the trace can be GC'd after this returns.
func (st *StreamingTrainer) AddTrace(t *Trace) {
	// Grow encoder vocabulary online.
	st.state.NodeEncoder.Extend(TraceFeatures(t))

	// Grow metadata vocabularies online.
	for _, s := range t.Spans {
		if s.Metadata == nil {
			continue
		}
		for _, col := range st.dependentAttributes {
			val := s.Metadata[col]
			m := st.vocabIdx[col]
			if _, ok := m[val]; !ok {
				m[val] = len(m)
				st.state.DependentAttributeVocabs[col] = append(st.state.DependentAttributeVocabs[col], val)
			}
		}
	}

	// Index this trace.
	it, minStart := indexTrace(t, st.state.NodeEncoder)
	if minStart < st.globalMinStart {
		st.globalMinStart = minStart
	}
	if minStart > st.globalMaxStart {
		st.globalMaxStart = minStart
	}

	// Decide trace type for stratified sampling.
	traceType := TraceTypeNormal
	if it.IsError && st.state.Config.StratifiedSampling {
		traceType = TraceTypeError
	}

	childCounts := childCountsOf(it)

	// Train root counts and collect duration samples.
	for spanID, s := range it.Spans {
		_, parentExists := it.Spans[s.ParentSpanID]
		if s.ParentSpanID == "" || !parentExists {
			cc := childCounts[spanID]
			feature := NodeFeature{
				NodeIdx:    s.NodeIdx,
				ChildIdx:   0,
				ChildCount: cc,
			}
			st.state.StartTableModel.AddRoot(traceType, feature)
			st.durationSamples[feature] = append(st.durationSamples[feature], float64(s.Duration))
		}
	}

	trainTopologyFromTrace(st.state, it, traceType, childCounts, &st.maxTreeSize)
	trainBoundsFromTrace(st.state, it)
	trainMetadataFromTrace(st.state, it, st.dependentAttributes)

	st.traceCount++
}

// Finalize fits final models in parallel (GMM, candidate cache, metadata
// predictor) and returns the completed state. Must be called exactly once.
func (st *StreamingTrainer) Finalize() *TPackModelState {
	st.state.MinStartTimeUs = st.globalMinStart
	st.state.MaxStartTimeUs = st.globalMaxStart
	st.state.TraceCount = st.traceCount

	if st.maxTreeSize > st.state.TopologyModel.MaxNodes {
		st.state.TopologyModel.MaxNodes = st.maxTreeSize
	}

	t0 := time.Now()
	var fitDur, cacheDur, metaDur time.Duration
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		t := time.Now()
		st.state.RootDurationModel.FitFromSamples(st.durationSamples)
		st.durationSamples = nil
		fitDur = time.Since(t)
	}()
	go func() {
		defer wg.Done()
		t := time.Now()
		st.state.TopologyModel.BuildChildCandidatesCache()
		cacheDur = time.Since(t)
	}()
	go func() {
		defer wg.Done()
		t := time.Now()
		finalizeMetadataPredictor(st.state)
		metaDur = time.Since(t)
	}()
	wg.Wait()
	log.Printf("    Finalize breakdown: FitGMM=%.1fs, BuildCache=%.1fs, DependentAttributePredictor=%.1fs, wall=%.1fs",
		fitDur.Seconds(), cacheDur.Seconds(), metaDur.Seconds(), time.Since(t0).Seconds())

	return st.state
}

// MergeTrainers combines independently-trained streaming trainers into one.
// Each trainer has its own NodeEncoder with local indices; the merge unifies
// vocabularies and remaps every dependent model. Returns the destination
// trainer (== trainers[0], modified in place); call Finalize on the result.
//
// Returns nil if trainers is empty.
func MergeTrainers(trainers []*StreamingTrainer) *StreamingTrainer {
	if len(trainers) == 0 {
		return nil
	}
	dst := trainers[0]

	for i := 1; i < len(trainers); i++ {
		src := trainers[i]

		// Merge encoder, get remap table for src's local indices.
		remap := dst.state.NodeEncoder.MergeFrom(src.state.NodeEncoder)

		// Remap src's model state to global indices.
		src.state.StartTableModel.RemapNodeIdx(remap)
		src.state.TopologyModel.RemapNodeIdx(remap)
		src.state.SpanDurationBounds.RemapNodeIdx(remap)
		src.state.SpanGapBounds.RemapNodeIdx(remap)
		if sp, ok := src.state.DependentAttributePredictor.(*StatisticalDependentAttributePredictor); ok {
			sp.RemapNodeIdx(remap)
		}

		// Remap durationSamples keys.
		remappedSamples := make(map[NodeFeature][]float64, len(src.durationSamples))
		for f, samples := range src.durationSamples {
			remappedSamples[RemapNodeFeature(f, remap)] = samples
		}

		// Merge into dst.
		dst.state.StartTableModel.MergeFrom(src.state.StartTableModel)
		dst.state.TopologyModel.MergeFrom(src.state.TopologyModel)
		dst.state.SpanDurationBounds.MergeFrom(src.state.SpanDurationBounds)
		dst.state.SpanGapBounds.MergeFrom(src.state.SpanGapBounds)
		if dstSP, ok := dst.state.DependentAttributePredictor.(*StatisticalDependentAttributePredictor); ok {
			if srcSP, ok := src.state.DependentAttributePredictor.(*StatisticalDependentAttributePredictor); ok {
				dstSP.MergeFrom(srcSP)
			}
		}

		// Merge duration samples.
		for f, samples := range remappedSamples {
			dst.durationSamples[f] = append(dst.durationSamples[f], samples...)
		}

		// Merge metadata vocabs.
		for _, col := range dst.dependentAttributes {
			srcMap := src.vocabIdx[col]
			dstMap := dst.vocabIdx[col]
			for val := range srcMap {
				if _, ok := dstMap[val]; !ok {
					dstMap[val] = len(dstMap)
					dst.state.DependentAttributeVocabs[col] = append(dst.state.DependentAttributeVocabs[col], val)
				}
			}
		}

		// Merge global timestamps.
		if src.globalMinStart < dst.globalMinStart {
			dst.globalMinStart = src.globalMinStart
		}
		if src.globalMaxStart > dst.globalMaxStart {
			dst.globalMaxStart = src.globalMaxStart
		}
		dst.traceCount += src.traceCount
		if src.maxTreeSize > dst.maxTreeSize {
			dst.maxTreeSize = src.maxTreeSize
		}
	}

	return dst
}

// TrainBucket is a convenience for the simple single-bucket case: build a
// fresh trainer, add every trace, and return the finalized state.
//
// As a side-effect, traces[i] is set to nil after AddTrace to allow GC.
func TrainBucket(traces []*Trace, cfg TPackConfig, primaryAttributes, dependentAttributes []string) (*TPackModelState, error) {
	st := NewStreamingTrainer(cfg, primaryAttributes, dependentAttributes)
	for i, t := range traces {
		st.AddTrace(t)
		traces[i] = nil
	}
	return st.Finalize(), nil
}
