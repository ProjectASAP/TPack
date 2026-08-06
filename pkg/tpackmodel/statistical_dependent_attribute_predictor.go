package tpackmodel

import (
	"math"
	"math/rand"
	"sort"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// nodePairKey identifies a (parent_node_idx, child_node_idx) pair.
type nodePairKey struct {
	ParentNodeIdx int32
	ChildNodeIdx  int32
}

// regressionCoeffs holds the fitted linear regression for one response variable:
//
//	y = Beta0 + Beta1*normalizedChildIdx
type regressionCoeffs struct {
	Beta0 float64
	Beta1 float64
}

// regressionAccumulator accumulates sufficient statistics for 1-predictor OLS:
//
//	y = β₀ + β₂·x  (x = normalizedChildIdx)
type regressionAccumulator struct {
	N     float64
	SumX  float64 // Σ normalizedChildIdx
	SumY  float64
	SumXX float64
	SumXY float64
	SumYY float64
}

func (a *regressionAccumulator) add(x, y float64) {
	a.N++
	a.SumX += x
	a.SumY += y
	a.SumXX += x * x
	a.SumXY += x * y
	a.SumYY += y * y
}

// solve fits the 1-predictor OLS regression and returns coefficients.
// Returns the unconditional mean fallback if n < minN or if the normal equations are singular.
func (a *regressionAccumulator) solve(minN int) regressionCoeffs {
	n := a.N
	if int(n) < minN {
		mean := 0.0
		if n > 0 {
			mean = a.SumY / n
		}
		return regressionCoeffs{Beta0: mean}
	}

	// Normal equations for y = β₀ + β₂·x:
	// | n    Σx  | | β₀ |   | Σy  |
	// | Σx   Σx² | | β₂ | = | Σxy |
	det := n*a.SumXX - a.SumX*a.SumX
	if math.Abs(det) < 1e-15 {
		mean := a.SumY / n
		return regressionCoeffs{Beta0: mean}
	}

	beta0 := (a.SumY*a.SumXX - a.SumX*a.SumXY) / det
	beta1 := (n*a.SumXY - a.SumX*a.SumY) / det

	return regressionCoeffs{Beta0: beta0, Beta1: beta1}
}

// nodePairStats holds fitted statistics for a (parent, child) node pair.
type nodePairStats struct {
	// Regression coefficients (conditioned on logParentDur + normalizedChildIdx)
	GapRegression regressionCoeffs
	DurRegression regressionCoeffs

	// Percentile fields (used when OffsetParam == "percentile")
	GapPercentiles []float64 // 21 values: p0,p5,...,p100 in absolute µs
	DurPercentiles []float64

	// Memorized raw (gapRatio, durRatio) pairs for small groups (≤memorizeThreshold)
	MemorizedGaps []float64 // non-nil when count <= memorizeThreshold
	MemorizedDurs []float64 // jointly indexed with MemorizedGaps
	memorizeIdx   int       // cursor for cycling through memorized values

	CategoricalProbs [][]float64 // [colIdx][valueIdx] — one distribution per metadata column
	SampleCount      int
}

// nodePairAccumulator accumulates raw samples during training.
type nodePairAccumulator struct {
	gapAccum regressionAccumulator
	durAccum regressionAccumulator
	rawGaps  []float64 // raw values (ratios or absolute µs depending on mode)
	rawDurs  []float64
	catCounts [][]float64 // [colIdx][valIdx]
	count     int
}

// StatisticalDependentAttributePredictor uses linear regression per (parent, child)
// node pair, conditioned on log(parentDuration) and normalizedChildIdx.
type StatisticalDependentAttributePredictor struct {
	Config      TPackConfig
	Rng         *rand.Rand
	NumMetaCols int // number of metadata columns being tracked

	// stats[(parent_node_idx, child_node_idx)] = stats (fitted)
	Stats map[nodePairKey]*nodePairStats

	// accumulators are used during training (cleared after FinalizeFit)
	accumulators map[nodePairKey]*nodePairAccumulator
}

// NewStatisticalDependentAttributePredictor creates a new statistical predictor for the
// given number of metadata columns. The column count is fixed at construction
// because the predictor's per-edge stats and protobuf round-trip both depend
// on it being consistent across train, save, load, and sample.
func NewStatisticalDependentAttributePredictor(config TPackConfig, numMetaCols int, rng *rand.Rand) *StatisticalDependentAttributePredictor {
	return &StatisticalDependentAttributePredictor{
		Config:       config,
		Rng:          rng,
		NumMetaCols:  numMetaCols,
		Stats:        make(map[nodePairKey]*nodePairStats),
		accumulators: make(map[nodePairKey]*nodePairAccumulator),
	}
}

const regressionMinSamples = 5
const memorizeThreshold = 20

// AddSample records a training observation.
// metadataIndices has one entry per metadata column (index into that column's vocab).
func (p *StatisticalDependentAttributePredictor) AddSample(
	parentNodeIdx, childNodeIdx int32,
	gapRatio, durationRatio float64,
	parentDuration float64,
	normalizedChildIdx float64,
	metadataIndices []int,
) {
	key := nodePairKey{parentNodeIdx, childNodeIdx}
	acc, ok := p.accumulators[key]
	if !ok {
		acc = &nodePairAccumulator{
			catCounts: make([][]float64, p.NumMetaCols),
		}
		p.accumulators[key] = acc
	}

	if p.Config.OffsetModel == "percentile" {
		// Percentile mode: accumulate all values (no cap)
		acc.rawGaps = append(acc.rawGaps, gapRatio)
		acc.rawDurs = append(acc.rawDurs, durationRatio)
	} else {
		// Collect raw values for potential memorization of small groups
		if len(acc.rawGaps) <= memorizeThreshold {
			acc.rawGaps = append(acc.rawGaps, gapRatio)
			acc.rawDurs = append(acc.rawDurs, durationRatio)
		}
		acc.gapAccum.add(normalizedChildIdx, gapRatio)
		acc.durAccum.add(normalizedChildIdx, durationRatio)
	}

	for colIdx, valIdx := range metadataIndices {
		if colIdx >= p.NumMetaCols {
			break
		}
		for valIdx >= len(acc.catCounts[colIdx]) {
			acc.catCounts[colIdx] = append(acc.catCounts[colIdx], 0)
		}
		if valIdx >= 0 {
			acc.catCounts[colIdx][valIdx]++
		}
	}
	acc.count++
}

// RemapNodeIdx translates all nodePairKey references using the given mapping.
func (p *StatisticalDependentAttributePredictor) RemapNodeIdx(mapping []int32) {
	remapped := make(map[nodePairKey]*nodePairAccumulator, len(p.accumulators))
	for k, acc := range p.accumulators {
		if int(k.ParentNodeIdx) < len(mapping) {
			k.ParentNodeIdx = mapping[k.ParentNodeIdx]
		}
		if int(k.ChildNodeIdx) < len(mapping) {
			k.ChildNodeIdx = mapping[k.ChildNodeIdx]
		}
		if existing, ok := remapped[k]; ok {
			mergeAccumulators(existing, acc)
		} else {
			remapped[k] = acc
		}
	}
	p.accumulators = remapped
}

// MergeFrom combines another StatisticalDependentAttributePredictor's accumulators into this one.
func (p *StatisticalDependentAttributePredictor) MergeFrom(other *StatisticalDependentAttributePredictor) {
	for k, acc := range other.accumulators {
		if existing, ok := p.accumulators[k]; ok {
			mergeAccumulators(existing, acc)
		} else {
			p.accumulators[k] = acc
		}
	}
}

func mergeAccumulators(dst, src *nodePairAccumulator) {
	dst.gapAccum.N += src.gapAccum.N
	dst.gapAccum.SumX += src.gapAccum.SumX
	dst.gapAccum.SumY += src.gapAccum.SumY
	dst.gapAccum.SumXX += src.gapAccum.SumXX
	dst.gapAccum.SumXY += src.gapAccum.SumXY
	dst.gapAccum.SumYY += src.gapAccum.SumYY

	dst.durAccum.N += src.durAccum.N
	dst.durAccum.SumX += src.durAccum.SumX
	dst.durAccum.SumY += src.durAccum.SumY
	dst.durAccum.SumXX += src.durAccum.SumXX
	dst.durAccum.SumXY += src.durAccum.SumXY
	dst.durAccum.SumYY += src.durAccum.SumYY

	dst.rawGaps = append(dst.rawGaps, src.rawGaps...)
	dst.rawDurs = append(dst.rawDurs, src.rawDurs...)
	dst.count += src.count

	// Merge categorical counts (extend if needed)
	for colIdx, srcCounts := range src.catCounts {
		if colIdx >= len(dst.catCounts) {
			dst.catCounts = append(dst.catCounts, make([][]float64, colIdx-len(dst.catCounts)+1)...)
		}
		for valIdx, c := range srcCounts {
			for valIdx >= len(dst.catCounts[colIdx]) {
				dst.catCounts[colIdx] = append(dst.catCounts[colIdx], 0)
			}
			dst.catCounts[colIdx][valIdx] += c
		}
	}
}

// FinalizeFit converts accumulated statistics to fitted models.
// Must be called after all AddSample calls.
func (p *StatisticalDependentAttributePredictor) FinalizeFit() {
	for key, acc := range p.accumulators {
		stats := &nodePairStats{
			SampleCount: acc.count,
		}

		if acc.count <= memorizeThreshold {
			stats.MemorizedGaps = make([]float64, len(acc.rawGaps))
			copy(stats.MemorizedGaps, acc.rawGaps)
			stats.MemorizedDurs = make([]float64, len(acc.rawDurs))
			copy(stats.MemorizedDurs, acc.rawDurs)
			// Joint shuffle — same permutation for both slices
			p.Rng.Shuffle(len(stats.MemorizedGaps), func(i, j int) {
				stats.MemorizedGaps[i], stats.MemorizedGaps[j] = stats.MemorizedGaps[j], stats.MemorizedGaps[i]
				stats.MemorizedDurs[i], stats.MemorizedDurs[j] = stats.MemorizedDurs[j], stats.MemorizedDurs[i]
			})
		} else if p.Config.OffsetModel == "percentile" {
			// Percentile mode: compute empirical percentiles from absolute values
			gaps := make([]float64, len(acc.rawGaps))
			copy(gaps, acc.rawGaps)
			durs := make([]float64, len(acc.rawDurs))
			copy(durs, acc.rawDurs)
			sort.Float64s(gaps)
			sort.Float64s(durs)
			stats.GapPercentiles = computePercentiles(gaps)
			stats.DurPercentiles = computePercentiles(durs)
		} else {
			// Solve regressions
			gapReg := acc.gapAccum.solve(regressionMinSamples)
			durReg := acc.durAccum.solve(regressionMinSamples)
			stats.GapRegression = gapReg
			stats.DurRegression = durReg
		}

		// Normalize categorical counts to probabilities
		stats.CategoricalProbs = make([][]float64, p.NumMetaCols)
		for colIdx := 0; colIdx < p.NumMetaCols; colIdx++ {
			if colIdx < len(acc.catCounts) && len(acc.catCounts[colIdx]) > 0 {
				total := 0.0
				for _, c := range acc.catCounts[colIdx] {
					total += c
				}
				probs := make([]float64, len(acc.catCounts[colIdx]))
				if total > 0 {
					for i, c := range acc.catCounts[colIdx] {
						probs[i] = c / total
					}
				} else if len(probs) > 0 {
					probs[0] = 1.0
				}
				stats.CategoricalProbs[colIdx] = probs
			}
		}

		p.Stats[key] = stats
	}

	// Free accumulators
	p.accumulators = nil
}

// SampleBatch generates metadata for a batch of requests.
func (p *StatisticalDependentAttributePredictor) SampleBatch(
	requests []MetadataSampleRequest,
	durationBounds []MinMax,
	gapBounds []MinMax,
	rng *rand.Rand,
) []MetadataSampleResult {
	results := make([]MetadataSampleResult, len(requests))

	for i, req := range requests {
		stats := p.getStats(req.ParentFeature.NodeIdx, req.ChildFeature.NodeIdx)

		var gapRatio, durationRatio float64
		metaIndices := make([]int, p.NumMetaCols)

		if stats != nil {
			if stats.MemorizedGaps != nil {
				idx := stats.memorizeIdx
				if idx >= len(stats.MemorizedGaps) {
					// Reshuffle jointly, reset cursor
					rng.Shuffle(len(stats.MemorizedGaps), func(i, j int) {
						stats.MemorizedGaps[i], stats.MemorizedGaps[j] = stats.MemorizedGaps[j], stats.MemorizedGaps[i]
						stats.MemorizedDurs[i], stats.MemorizedDurs[j] = stats.MemorizedDurs[j], stats.MemorizedDurs[i]
					})
					idx = 0
				}
				gapRatio = stats.MemorizedGaps[idx]
				durationRatio = stats.MemorizedDurs[idx]
				stats.memorizeIdx = idx + 1
			} else if p.Config.OffsetModel == "percentile" {
				// Percentile mode: sample via piecewise-linear interpolation
				sampled := samplePercentileInterp(stats.GapPercentiles, rng)
				sampledDur := samplePercentileInterp(stats.DurPercentiles, rng)

				if p.Config.OffsetValue == "absolute" {
					// Absolute percentiles: convert to ratio
					if req.ParentDuration > 0 {
						gapRatio = sampled / req.ParentDuration
						durationRatio = sampledDur / req.ParentDuration
					}
				} else {
					// Ratio percentiles: use directly
					gapRatio = sampled
					durationRatio = sampledDur
				}
			} else {
				childIdx := req.NormalizedChildIdx
				gapRatio = stats.GapRegression.Beta0 +
					stats.GapRegression.Beta1*childIdx
				durationRatio = stats.DurRegression.Beta0 +
					stats.DurRegression.Beta1*childIdx
			}

			for colIdx := 0; colIdx < p.NumMetaCols; colIdx++ {
				if colIdx < len(stats.CategoricalProbs) && len(stats.CategoricalProbs[colIdx]) > 0 {
					metaIndices[colIdx] = sampleCategorical(stats.CategoricalProbs[colIdx], rng)
				}
			}
		} else {
			gapRatio = 0.1 + rng.Float64()*0.3
			durationRatio = 0.1 + rng.Float64()*0.3
		}

		memorized := stats != nil && stats.MemorizedGaps != nil
		if !memorized && p.Config.OffsetValue == "absolute" && p.Config.OffsetModel == "regression" {
			// Log parameterization: values are log-space, clamp to [0, ∞)
			gapRatio = math.Max(0, gapRatio)
			durationRatio = math.Max(0, durationRatio)

			// Apply bounds in absolute space, then convert back to log
			if durationBounds != nil && i < len(durationBounds) {
				db := durationBounds[i]
				absDur := math.Exp(math.Min(durationRatio, 30)) - 1
				absDur = math.Max(db.Min, math.Min(db.Max, absDur))
				durationRatio = math.Log(absDur + 1)
			}

			if gapBounds != nil && i < len(gapBounds) {
				gb := gapBounds[i]
				absGap := math.Exp(math.Min(gapRatio, 30)) - 1
				absGap = math.Max(gb.Min, math.Min(gb.Max, absGap))
				gapRatio = math.Log(absGap + 1)
			}
		} else if !memorized {
			// Ratio space (ratio+regression, ratio+percentile, absolute+percentile all output ratios)
			gapRatio = math.Max(0, math.Min(1, gapRatio))
			durationRatio = math.Max(0, math.Min(1, durationRatio))

			// Apply bounds in absolute space
			if durationBounds != nil && i < len(durationBounds) {
				db := durationBounds[i]
				childDur := durationRatio * req.ParentDuration
				if childDur < db.Min {
					durationRatio = db.Min / req.ParentDuration
				} else if childDur > db.Max {
					durationRatio = db.Max / req.ParentDuration
				}
				durationRatio = math.Max(0, math.Min(1, durationRatio))
			}

			if gapBounds != nil && i < len(gapBounds) {
				gb := gapBounds[i]
				gap := gapRatio * req.ParentDuration
				if gap < gb.Min {
					gapRatio = gb.Min / req.ParentDuration
				} else if gap > gb.Max {
					gapRatio = gb.Max / req.ParentDuration
				}
				gapRatio = math.Max(0, math.Min(1, gapRatio))
			}

			// Ensure gap + duration <= 1.0
			if gapRatio+durationRatio > 1.0 {
				total := gapRatio + durationRatio
				gapRatio /= total
				durationRatio /= total
			}
		}

		results[i] = MetadataSampleResult{
			GapRatio:        gapRatio,
			DurationRatio:   durationRatio,
			MetadataIndices: metaIndices,
		}
	}

	return results
}

// getStats returns the stats for a (parent, child) pair.
func (p *StatisticalDependentAttributePredictor) getStats(parentIdx, childIdx int32) *nodePairStats {
	key := nodePairKey{parentIdx, childIdx}
	if stats, ok := p.Stats[key]; ok {
		return stats
	}
	return nil
}

// sampleCategorical samples an index from a probability distribution.
func sampleCategorical(probs []float64, rng *rand.Rand) int {
	r := rng.Float64()
	cumProb := 0.0
	for i, p := range probs {
		cumProb += p
		if r <= cumProb {
			return i
		}
	}
	return len(probs) - 1
}

// computePercentiles computes p0,p5,p10,...,p95,p100 (21 values) from a SORTED slice.
func computePercentiles(sorted []float64) []float64 {
	const numPct = 21
	result := make([]float64, numPct)
	n := len(sorted)
	if n == 0 {
		return result
	}
	for i := range numPct {
		p := float64(i) * 5.0 / 100.0
		idx := p * float64(n-1)
		lo := int(math.Floor(idx))
		hi := int(math.Ceil(idx))
		if hi >= n {
			hi = n - 1
		}
		frac := idx - float64(lo)
		result[i] = sorted[lo]*(1-frac) + sorted[hi]*frac
	}
	return result
}

// samplePercentileInterp draws from piecewise-linear CDF over 21 percentiles.
func samplePercentileInterp(pct []float64, rng *rand.Rand) float64 {
	if len(pct) == 0 {
		return 0
	}
	u := rng.Float64()
	idx := u * float64(len(pct)-1)
	lo := int(math.Floor(idx))
	hi := lo + 1
	if hi >= len(pct) {
		return pct[len(pct)-1]
	}
	frac := idx - float64(lo)
	return pct[lo]*(1-frac) + pct[hi]*frac
}

// SaveStateDict writes the statistical predictor to protobuf.
func (p *StatisticalDependentAttributePredictor) SaveStateDict(models *pb.TPackModels) {
	for key, stats := range p.Stats {
		sm := &pb.StatisticalDependentAttributeModel{
			ParentNodeIdx: key.ParentNodeIdx,
			ChildNodeIdx:  key.ChildNodeIdx,
			SampleCount:   int32(stats.SampleCount),
			GapBeta0: stats.GapRegression.Beta0,
			GapBeta1: stats.GapRegression.Beta1,
			DurBeta0: stats.DurRegression.Beta0,
			DurBeta1: stats.DurRegression.Beta1,
			// Percentile fields
			GapPercentiles: stats.GapPercentiles,
			DurPercentiles: stats.DurPercentiles,
		}
		sm.MemorizedGaps = stats.MemorizedGaps
		sm.MemorizedDurs = stats.MemorizedDurs
		for _, probs := range stats.CategoricalProbs {
			sm.DependentAttributeProbs = append(sm.DependentAttributeProbs, &pb.DependentAttributeProbs{
				Probs: probs,
			})
		}
		models.StatisticalDependentAttributes = append(models.StatisticalDependentAttributes, sm)
	}
}

// LoadStateDict restores the statistical predictor from protobuf.
func (p *StatisticalDependentAttributePredictor) LoadStateDict(models *pb.TPackModels) {
	p.Stats = make(map[nodePairKey]*nodePairStats)

	numCols := p.NumMetaCols

	for _, sm := range models.StatisticalDependentAttributes {
		key := nodePairKey{sm.ParentNodeIdx, sm.ChildNodeIdx}
		stats := &nodePairStats{
			SampleCount: int(sm.SampleCount),
			GapRegression: regressionCoeffs{
				Beta0: sm.GapBeta0,
				Beta1: sm.GapBeta1,
			},
			DurRegression: regressionCoeffs{
				Beta0: sm.DurBeta0,
				Beta1: sm.DurBeta1,
			},
			GapPercentiles: sm.GapPercentiles,
			DurPercentiles: sm.DurPercentiles,
		}

		if len(sm.MemorizedGaps) > 0 {
			stats.MemorizedGaps = sm.MemorizedGaps
			stats.MemorizedDurs = sm.MemorizedDurs
			// Shuffle on load so cycling starts fresh
			p.Rng.Shuffle(len(stats.MemorizedGaps), func(i, j int) {
				stats.MemorizedGaps[i], stats.MemorizedGaps[j] = stats.MemorizedGaps[j], stats.MemorizedGaps[i]
				stats.MemorizedDurs[i], stats.MemorizedDurs[j] = stats.MemorizedDurs[j], stats.MemorizedDurs[i]
			})
		}

		if len(sm.DependentAttributeProbs) > 0 {
			stats.CategoricalProbs = make([][]float64, len(sm.DependentAttributeProbs))
			for i, mcp := range sm.DependentAttributeProbs {
				stats.CategoricalProbs[i] = mcp.Probs
			}
		} else {
			stats.CategoricalProbs = make([][]float64, numCols)
		}

		p.Stats[key] = stats
	}
}
