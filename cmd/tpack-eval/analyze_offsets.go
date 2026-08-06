package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// offsetPairKey identifies a (parent node, child node) pair for offset analysis.
type offsetPairKey struct {
	ParentNodeIdx int32
	ChildNodeIdx  int32
}

// ---------------------------------------------------------------------------
// 1-predictor OLS: y = beta0 + beta1*x  (x = normalizedChildIdx)
// ---------------------------------------------------------------------------

type offsetRegAccum struct {
	N     float64
	SumX  float64
	SumY  float64
	SumXX float64
	SumXY float64
	SumYY float64
}

func (a *offsetRegAccum) add(x, y float64) {
	a.N++
	a.SumX += x
	a.SumY += y
	a.SumXX += x * x
	a.SumXY += x * y
	a.SumYY += y * y
}

type offsetRegCoeffs struct {
	Beta0 float64
	Beta1 float64
}

func (a *offsetRegAccum) solve() offsetRegCoeffs {
	n := a.N
	if n < 2 {
		mean := 0.0
		if n > 0 {
			mean = a.SumY / n
		}
		return offsetRegCoeffs{Beta0: mean}
	}
	denom := n*a.SumXX - a.SumX*a.SumX
	if math.Abs(denom) < 1e-15 {
		return offsetRegCoeffs{Beta0: a.SumY / n}
	}
	beta1 := (n*a.SumXY - a.SumX*a.SumY) / denom
	beta0 := (a.SumY - beta1*a.SumX) / n

	return offsetRegCoeffs{Beta0: beta0, Beta1: beta1}
}

// ---------------------------------------------------------------------------
// Data structures
// ---------------------------------------------------------------------------

type bucketPairKey struct {
	BucketKey     int64
	ParentNodeIdx int32
	ChildNodeIdx  int32
}

type offsetSample struct {
	parentDur    float64
	childDur     float64
	gap          float64
	childIdx     float64
	childNodeIdx int32
}

type bucketPairData struct {
	gapRatioAccum offsetRegAccum // target: gap / parentDur
	logGapAccum   offsetRegAccum // target: log(gap + 1)
	durRatioAccum offsetRegAccum // target: childDur / parentDur
	logDurAccum   offsetRegAccum // target: log(childDur + 1)

	// Raw samples for distributional evaluation
	samples []offsetSample
}

// ---------------------------------------------------------------------------
// Reconstruction helpers
// ---------------------------------------------------------------------------

// reconstructGap predicts gap from regression coefficients in the given parameterization.
func reconstructGap(param string, c offsetRegCoeffs, parentDur, childIdx float64) float64 {
	pred := c.Beta0 + c.Beta1*childIdx
	switch param {
	case "gapRatio":
		return clamp(pred, 0, 1) * parentDur
	case "logGap":
		return clamp(math.Exp(clamp(pred, 0, 30))-1, 0, parentDur)
	}
	return 0
}

// reconstructDur predicts child duration from regression coefficients in the given parameterization.
func reconstructDur(param string, c offsetRegCoeffs, parentDur, childIdx float64) float64 {
	pred := c.Beta0 + c.Beta1*childIdx
	switch param {
	case "durRatio":
		return clamp(pred, 0, 1) * parentDur
	case "logDur":
		return clamp(math.Exp(clamp(pred, 0, 30))-1, 0, parentDur)
	}
	return 0
}

// reconstructWithBounds predicts gap and duration with bounds applied, matching the pipeline.
// For ratio mode: clamp ratios to [0,1], apply bounds, then proportional scaling if gap+dur > parentDur.
// For log mode: convert to absolute, apply bounds, clamp to [0, parentDur].
func reconstructWithBounds(
	gapCoeffs, durCoeffs offsetRegCoeffs,
	parentDur, childIdx float64,
	durMin, durMax, gapMin, gapMax float64,
	isLog bool,
) (gap, dur float64) {
	gapPred := gapCoeffs.Beta0 + gapCoeffs.Beta1*childIdx
	durPred := durCoeffs.Beta0 + durCoeffs.Beta1*childIdx

	if isLog {
		// Log mode: clamp pred to [0, ∞), convert to absolute, apply bounds
		gap = math.Exp(clamp(gapPred, 0, 30)) - 1
		dur = math.Exp(clamp(durPred, 0, 30)) - 1

		// Apply bounds
		gap = clamp(gap, gapMin, gapMax)
		dur = clamp(dur, durMin, durMax)

		// Clamp to parent duration
		gap = clamp(gap, 0, parentDur)
		dur = clamp(dur, 0, parentDur)
	} else {
		// Ratio mode: clamp to [0, 1]
		gapRatio := clamp(gapPred, 0, 1)
		durRatio := clamp(durPred, 0, 1)

		// Apply bounds in absolute space
		absGap := gapRatio * parentDur
		absDur := durRatio * parentDur

		absGap = clamp(absGap, gapMin, gapMax)
		absDur = clamp(absDur, durMin, durMax)

		gapRatio = clamp(absGap/parentDur, 0, 1)
		durRatio = clamp(absDur/parentDur, 0, 1)

		// Proportional scaling if gap+dur > 1
		if gapRatio+durRatio > 1.0 {
			total := gapRatio + durRatio
			gapRatio /= total
			durRatio /= total
		}

		gap = gapRatio * parentDur
		dur = durRatio * parentDur
	}

	// Minimum duration floor
	if dur < 1.0 {
		dur = 1.0
	}

	return gap, dur
}

// ---------------------------------------------------------------------------
// Main analysis
// ---------------------------------------------------------------------------

func runAnalyzeOffsets(inputPath string, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) error {
	t0 := time.Now()
	log.Printf("Reading %s ...", inputPath)
	buckets, err := readOTLP(inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	totalTraces := 0
	totalSpans := 0
	for _, traces := range buckets {
		totalTraces += len(traces)
		for _, t := range traces {
			totalSpans += len(t.Spans)
		}
	}
	log.Printf("Read %d traces (%d spans) in %d buckets [%.1fs]",
		totalTraces, totalSpans, len(buckets), time.Since(t0).Seconds())

	// Collect all traces and fit encoder
	var allTraces []*tpackmodel.Trace
	for _, traces := range buckets {
		allTraces = append(allTraces, traces...)
	}
	encoder := tpackmodel.NewNodeEncoder()
	encoder.Fit(tpackmodel.CollectFeatures(allTraces))
	log.Printf("Encoder fitted: %d node types", encoder.VocabSize())

	// Accumulate per-(bucket, pair) statistics
	bpData := make(map[bucketPairKey]*bucketPairData)

	for bk, traces := range buckets {
		for _, t := range traces {
			parentChildren := make(map[string][]*tpackmodel.Span)
			for _, s := range t.Spans {
				if s.ParentSpanID != "" {
					if _, ok := t.Spans[s.ParentSpanID]; ok {
						parentChildren[s.ParentSpanID] = append(parentChildren[s.ParentSpanID], s)
					}
				}
			}

			for parentID, children := range parentChildren {
				parent := t.Spans[parentID]
				if parent.Duration <= 0 {
					continue
				}

				parentNodeIdx := encoder.Transform(parent.Feature)
				parentDur := float64(parent.Duration)

				sort.Slice(children, func(i, j int) bool {
					return children[i].StartTime < children[j].StartTime
				})

				for ci, child := range children {
					childNodeIdx := encoder.Transform(child.Feature)
					childDur := float64(child.Duration)
					gap := float64(child.StartTime - parent.StartTime)

					normalizedIdx := 0.0
					if len(children) > 1 {
						normalizedIdx = float64(ci) / float64(len(children)-1)
					}

					logGap := math.Log(math.Max(0, gap) + 1)
					logDur := math.Log(math.Max(0, childDur) + 1)
					gapRatio := 0.0
					if parentDur > 0 {
						gapRatio = math.Max(0, gap) / parentDur
					}
					durRatio := 0.0
					if parentDur > 0 {
						durRatio = math.Max(0, childDur) / parentDur
					}

					key := bucketPairKey{bk, parentNodeIdx, childNodeIdx}
					pd, ok := bpData[key]
					if !ok {
						pd = &bucketPairData{}
						bpData[key] = pd
					}

					pd.gapRatioAccum.add(normalizedIdx, gapRatio)
					pd.logGapAccum.add(normalizedIdx, logGap)
					pd.durRatioAccum.add(normalizedIdx, durRatio)
					pd.logDurAccum.add(normalizedIdx, logDur)
					pd.samples = append(pd.samples, offsetSample{
						parentDur:    parentDur,
						childDur:     childDur,
						gap:          gap,
						childIdx:     normalizedIdx,
						childNodeIdx: childNodeIdx,
					})
				}
			}
		}
	}

	// Count unique pairs across buckets
	uniquePairs := make(map[offsetPairKey]bool)
	for key := range bpData {
		uniquePairs[offsetPairKey{key.ParentNodeIdx, key.ChildNodeIdx}] = true
	}
	log.Printf("Collected %d (bucket, pair) groups from %d unique pairs across %d buckets",
		len(bpData), len(uniquePairs), len(buckets))

	const numPipelines = 2
	pipelineNames := [numPipelines]string{"ratio", "log"}

	type pairResult struct {
		key         offsetPairKey
		sampleCount int
		bucketCount int

		// Distributional percentile MAPEs
		// Index: 0=ratio, 1=log, 2=mpq
		p50GapMAPE [numPipelines]float64
		p90GapMAPE [numPipelines]float64
		p99GapMAPE [numPipelines]float64
		p50DurMAPE [numPipelines]float64
		p90DurMAPE [numPipelines]float64
		p99DurMAPE [numPipelines]float64
	}

	pairAccum := make(map[offsetPairKey]*pairResult)
	totalBPGroups := 0
	memorizedBPGroups := 0

	for bpKey, pd := range bpData {

		pairKey := offsetPairKey{bpKey.ParentNodeIdx, bpKey.ChildNodeIdx}

		// Solve OLS coefficients
		gapRatioCoeffs := pd.gapRatioAccum.solve()
		logGapCoeffs := pd.logGapAccum.solve()
		durRatioCoeffs := pd.durRatioAccum.solve()
		logDurCoeffs := pd.logDurAccum.solve()

		// Collect valid samples and build per-childNodeIdx bounds
		var validSamples []int
		durBounds := make(map[int32][2]float64) // [0]=min, [1]=max
		gapBoundsMap := make(map[int32][2]float64)
		for si, s := range pd.samples {
			if s.childDur <= 0 || s.gap < 0 {
				continue
			}
			validSamples = append(validSamples, si)

			if b, ok := durBounds[s.childNodeIdx]; ok {
				if s.childDur < b[0] {
					b[0] = s.childDur
				}
				if s.childDur > b[1] {
					b[1] = s.childDur
				}
				durBounds[s.childNodeIdx] = b
			} else {
				durBounds[s.childNodeIdx] = [2]float64{s.childDur, s.childDur}
			}

			if s.gap >= 0 {
				if b, ok := gapBoundsMap[s.childNodeIdx]; ok {
					if s.gap < b[0] {
						b[0] = s.gap
					}
					if s.gap > b[1] {
						b[1] = s.gap
					}
					gapBoundsMap[s.childNodeIdx] = b
				} else {
					gapBoundsMap[s.childNodeIdx] = [2]float64{s.gap, s.gap}
				}
			}
		}
		validCount := len(validSamples)
		if validCount == 0 {
			continue
		}
		totalBPGroups++
		if validCount <= 20 {
			memorizedBPGroups++
		}

		// Collect actual and generated distributions
		actualGaps := make([]float64, 0, validCount)
		actualDurs := make([]float64, 0, validCount)
		genGapsRatio := make([]float64, 0, validCount)
		genDursRatio := make([]float64, 0, validCount)
		genGapsLog := make([]float64, 0, validCount)
		genDursLog := make([]float64, 0, validCount)

		if validCount <= 20 {
			// Memorize: use actual values as generated for all pipelines
			for _, si := range validSamples {
				s := pd.samples[si]
				g := math.Max(0, s.gap)
				d := math.Max(0, s.childDur)
				actualGaps = append(actualGaps, g)
				actualDurs = append(actualDurs, d)
				genGapsRatio = append(genGapsRatio, g)
				genDursRatio = append(genDursRatio, d)
				genGapsLog = append(genGapsLog, g)
				genDursLog = append(genDursLog, d)
			}
		} else {
			for _, si := range validSamples {
				s := pd.samples[si]
				actualGaps = append(actualGaps, math.Max(0, s.gap))
				actualDurs = append(actualDurs, math.Max(0, s.childDur))

				db := durBounds[s.childNodeIdx]
				gb := gapBoundsMap[s.childNodeIdx]

				ratioGap, ratioDur := reconstructWithBounds(
					gapRatioCoeffs, durRatioCoeffs,
					s.parentDur, s.childIdx,
					db[0], db[1], gb[0], gb[1],
					false,
				)
				genGapsRatio = append(genGapsRatio, ratioGap)
				genDursRatio = append(genDursRatio, ratioDur)

				logGap, logDur := reconstructWithBounds(
					logGapCoeffs, logDurCoeffs,
					s.parentDur, s.childIdx,
					db[0], db[1], gb[0], gb[1],
					true,
				)
				genGapsLog = append(genGapsLog, logGap)
				genDursLog = append(genDursLog, logDur)
			}
		}

		sort.Float64s(actualGaps)
		sort.Float64s(actualDurs)
		sort.Float64s(genGapsRatio)
		sort.Float64s(genDursRatio)
		sort.Float64s(genGapsLog)
		sort.Float64s(genDursLog)

		var p50Gap, p90Gap, p99Gap [numPipelines]float64
		var p50Dur, p90Dur, p99Dur [numPipelines]float64

		genGaps := [numPipelines][]float64{genGapsRatio, genGapsLog}
		genDurs := [numPipelines][]float64{genDursRatio, genDursLog}

		for pi, pctl := range []float64{50, 90, 99} {
			actGapP := percentile(actualGaps, pctl)
			actDurP := percentile(actualDurs, pctl)

			var gapMAPE, durMAPE [numPipelines]float64
			for mi := range numPipelines {
				if actGapP > 0 {
					gapMAPE[mi] = math.Abs(percentile(genGaps[mi], pctl)-actGapP) / actGapP
				}
				if actDurP > 0 {
					durMAPE[mi] = math.Abs(percentile(genDurs[mi], pctl)-actDurP) / actDurP
				}
			}

			switch pi {
			case 0:
				p50Gap = gapMAPE
				p50Dur = durMAPE
			case 1:
				p90Gap = gapMAPE
				p90Dur = durMAPE
			case 2:
				p99Gap = gapMAPE
				p99Dur = durMAPE
			}
		}

		// Accumulate into per-pair result (weighted by sample count)
		pr, ok := pairAccum[pairKey]
		if !ok {
			pr = &pairResult{key: pairKey}
			pairAccum[pairKey] = pr
		}

		pr.sampleCount += validCount
		pr.bucketCount++

		for pi := range numPipelines {
			pr.p50GapMAPE[pi] += p50Gap[pi]
			pr.p90GapMAPE[pi] += p90Gap[pi]
			pr.p99GapMAPE[pi] += p99Gap[pi]
			pr.p50DurMAPE[pi] += p50Dur[pi]
			pr.p90DurMAPE[pi] += p90Dur[pi]
			pr.p99DurMAPE[pi] += p99Dur[pi]
		}
	}

	// Normalize per-pair results and collect
	var results []pairResult
	for _, pr := range pairAccum {
		w := float64(pr.bucketCount)
		for pi := range numPipelines {
			pr.p50GapMAPE[pi] /= w
			pr.p90GapMAPE[pi] /= w
			pr.p99GapMAPE[pi] /= w
			pr.p50DurMAPE[pi] /= w
			pr.p90DurMAPE[pi] /= w
			pr.p99DurMAPE[pi] /= w
		}
		results = append(results, *pr)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].sampleCount > results[j].sampleCount
	})

	log.Printf("Analyzed %d pairs (per-bucket regressions)", len(results))

	// Compute simple averages across pairs (equal weight per pair)
	var wP50Gap, wP90Gap, wP99Gap [numPipelines]float64
	var wP50Dur, wP90Dur, wP99Dur [numPipelines]float64
	totalWeight := float64(len(results))

	for _, r := range results {
		for pi := range numPipelines {
			wP50Gap[pi] += r.p50GapMAPE[pi]
			wP90Gap[pi] += r.p90GapMAPE[pi]
			wP99Gap[pi] += r.p99GapMAPE[pi]
			wP50Dur[pi] += r.p50DurMAPE[pi]
			wP90Dur[pi] += r.p90DurMAPE[pi]
			wP99Dur[pi] += r.p99DurMAPE[pi]
		}
	}

	if totalWeight == 0 {
		return fmt.Errorf("no pairs with sufficient samples found")
	}

	tw := totalWeight

	// Print summary
	fmt.Printf("\n=== Offset Distributional Evaluation ===\n")
	fmt.Printf("Input: %s (%d traces, %d spans)\n", inputPath, totalTraces, totalSpans)
	fmt.Printf("Pairs analyzed: %d\n", len(results))
	fmt.Printf("Bucket-pair groups: %d (%d memorized, %d fitted)\n", totalBPGroups, memorizedBPGroups, totalBPGroups-memorizedBPGroups)
	fmt.Printf("Compares p50/p90/p99 of generated distribution vs actual distribution.\n\n")

	fmt.Printf("%-20s %10s %10s %10s %10s %10s %10s\n",
		"Pipeline", "p50 gap", "p90 gap", "p99 gap", "p50 dur", "p90 dur", "p99 dur")
	fmt.Printf("%-20s %10s %10s %10s %10s %10s %10s\n",
		"--------------------", "----------", "----------", "----------", "----------", "----------", "----------")
	for pi := range numPipelines {
		fmt.Printf("%-20s %9.2f%% %9.2f%% %9.2f%% %9.2f%% %9.2f%% %9.2f%%\n",
			pipelineNames[pi],
			wP50Gap[pi]/tw*100, wP90Gap[pi]/tw*100, wP99Gap[pi]/tw*100,
			wP50Dur[pi]/tw*100, wP90Dur[pi]/tw*100, wP99Dur[pi]/tw*100)
	}

	// Write per-pair TSV
	tsvPath := strings.TrimSuffix(inputPath, "/") + "_offset_analysis.tsv"
	f, err := os.Create(tsvPath)
	if err != nil {
		return fmt.Errorf("create TSV: %w", err)
	}
	defer f.Close()

	header := []string{"parentNodeIdx", "childNodeIdx", "sampleCount"}
	for _, pn := range pipelineNames {
		header = append(header,
			pn+"_p50GapMAPE", pn+"_p90GapMAPE", pn+"_p99GapMAPE",
			pn+"_p50DurMAPE", pn+"_p90DurMAPE", pn+"_p99DurMAPE")
	}
	fmt.Fprintln(f, strings.Join(header, "\t"))

	for _, r := range results {
		fields := []string{
			fmt.Sprintf("%d", r.key.ParentNodeIdx),
			fmt.Sprintf("%d", r.key.ChildNodeIdx),
			fmt.Sprintf("%d", r.sampleCount),
		}
		for pi := range numPipelines {
			fields = append(fields,
				fmt.Sprintf("%.6f", r.p50GapMAPE[pi]),
				fmt.Sprintf("%.6f", r.p90GapMAPE[pi]),
				fmt.Sprintf("%.6f", r.p99GapMAPE[pi]),
				fmt.Sprintf("%.6f", r.p50DurMAPE[pi]),
				fmt.Sprintf("%.6f", r.p90DurMAPE[pi]),
				fmt.Sprintf("%.6f", r.p99DurMAPE[pi]),
			)
		}
		fmt.Fprintln(f, strings.Join(fields, "\t"))
	}

	log.Printf("Wrote per-pair TSV to %s", tsvPath)

	// Write summary TSV
	summaryPath := strings.TrimSuffix(inputPath, "/") + "_offset_summary.tsv"
	sf, err := os.Create(summaryPath)
	if err != nil {
		return fmt.Errorf("create summary TSV: %w", err)
	}
	defer sf.Close()

	fmt.Fprintln(sf, "pipeline\tp50GapMAPE\tp90GapMAPE\tp99GapMAPE\tp50DurMAPE\tp90DurMAPE\tp99DurMAPE")
	for pi := range numPipelines {
		fmt.Fprintf(sf, "%s\t%.6f\t%.6f\t%.6f\t%.6f\t%.6f\t%.6f\n",
			pipelineNames[pi],
			wP50Gap[pi]/tw, wP90Gap[pi]/tw, wP99Gap[pi]/tw,
			wP50Dur[pi]/tw, wP90Dur[pi]/tw, wP99Dur[pi]/tw)
	}

	log.Printf("Wrote summary TSV to %s", summaryPath)
	return nil
}
