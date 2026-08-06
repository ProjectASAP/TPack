package tpackmodel

import (
	"math"
	mathrand "math/rand"
)

// generateShardCount runs the full generation pipeline but only counts spans
// (used for timing experiments). Processes in batches of 1000 traces to bound memory.
func generateShardCount(state *TPackModelState, rootSamples []RootSample, rng *mathrand.Rand) int {
	const batchSize = 1000
	total := 0
	for i := 0; i < len(rootSamples); i += batchSize {
		end := min(i+batchSize, len(rootSamples))
		_, count := generateShardInner(state, rootSamples[i:end], rng, true)
		total += count
	}
	return total
}

// generateShard generates traces for a subset of root samples. State is read-only.
func generateShard(state *TPackModelState, rootSamples []RootSample, rng *mathrand.Rand) []GeneratedSpan {
	spans, _ := generateShardInner(state, rootSamples, rng, false)
	return spans
}

func generateShardInner(state *TPackModelState, rootSamples []RootSample, rng *mathrand.Rand, countOnly bool) ([]GeneratedSpan, int) {
	vocabSize := state.NodeEncoder.VocabSize()

	// Stage 2-3: For each root, generate topology and root duration.
	type traceInfo struct {
		traceID      string
		rootTree     *TreeNode
		traceStart   int64
		rootDuration float64
	}

	var infos []traceInfo
	for _, sample := range rootSamples {
		traceID := RandomHex(rng, 32)

		var tree *TreeNode
		if sample.Template != nil {
			tree = state.TopologyModel.GenerateTreeFromTemplate(sample.Template)
		} else {
			tree = state.TopologyModel.GenerateTreeStructure(sample.Feature, sample.TraceType, rng)
		}
		if tree == nil {
			continue
		}

		rootDuration := state.RootDurationModel.SampleDuration(sample.Feature, rng, state.Config.UseDurationBounds)

		timeRange := state.MaxStartTimeUs - state.MinStartTimeUs
		if timeRange <= 0 {
			timeRange = 1
		}
		traceStart := state.MinStartTimeUs + rng.Int63n(timeRange)

		infos = append(infos, traceInfo{
			traceID:      traceID,
			rootTree:     tree,
			traceStart:   traceStart,
			rootDuration: rootDuration,
		})
	}

	// Stage 4: Generate metadata (batch by level).
	var allSpans []GeneratedSpan
	spanCount := 0

	// No roots survived (empty model or all trees nil) — nothing to do.
	// Avoids invoking a nil DependentAttributePredictor on a freshly-constructed state.
	if len(infos) == 0 {
		return allSpans, spanCount
	}

	// Collect root metadata requests with NO_PARENT sentinel.
	rootRequests := make([]MetadataSampleRequest, len(infos))
	for i, info := range infos {
		rootRequests[i] = MetadataSampleRequest{
			ParentFeature:      NodeFeature{NodeIdx: vocabSize}, // NO_PARENT
			ChildFeature:       info.rootTree.Feature,
			ParentDuration:     info.rootDuration,
			NormalizedChildIdx: 0.0,
		}
	}

	rootResults := state.DependentAttributePredictor.SampleBatch(rootRequests, nil, nil, rng)

	type levelItem struct {
		traceID   string
		treeNode  *TreeNode
		spanID    string
		startTime int64
		duration  float64
	}

	var currentLevel []levelItem

	for i, info := range infos {
		rootSpanID := RandomHex(rng, 16)
		meta := IndicesToMetadata(rootResults[i].MetadataIndices, state.DependentAttributes, state.DependentAttributeVocabs)

		spanCount++
		if !countOnly {
			allSpans = append(allSpans, GeneratedSpan{
				TraceID:      info.traceID,
				SpanID:       rootSpanID,
				ParentSpanID: "",
				NodeIdx:      info.rootTree.Feature.NodeIdx,
				StartTime:    info.traceStart,
				Duration:     int64(info.rootDuration),
				Metadata:     meta,
			})
		}

		currentLevel = append(currentLevel, levelItem{
			traceID:   info.traceID,
			treeNode:  info.rootTree,
			spanID:    rootSpanID,
			startTime: info.traceStart,
			duration:  info.rootDuration,
		})
	}

	// Process level by level.
	for len(currentLevel) > 0 {
		var nextLevel []levelItem
		var megaRequests []MetadataSampleRequest
		var megaDurationBounds []MinMax
		var megaGapBounds []MinMax

		type requestInfo struct {
			traceID      string
			childTree    *TreeNode
			childSpanID  string
			parentSpanID string
			parentStart  int64
			parentDur    float64
		}
		var requestInfos []requestInfo

		for _, item := range currentLevel {
			children := item.treeNode.Children
			childCount := len(children)

			for childIdx, childTree := range children {
				childSpanID := RandomHex(rng, 16)

				normalizedChildIdx := 0.0
				if childCount > 1 {
					normalizedChildIdx = float64(childIdx) / float64(childCount-1)
				}

				megaRequests = append(megaRequests, MetadataSampleRequest{
					ParentFeature:      item.treeNode.Feature,
					ChildFeature:       childTree.Feature,
					ParentDuration:     item.duration,
					NormalizedChildIdx: normalizedChildIdx,
				})

				if state.Config.UseDurationBounds {
					minDur, maxDur := state.SpanDurationBounds.GetDurationBounds(childTree.Feature.NodeIdx)
					megaDurationBounds = append(megaDurationBounds, MinMax{Min: minDur, Max: maxDur})
					minGap, maxGap := state.SpanGapBounds.GetGapBounds(childTree.Feature.NodeIdx)
					megaGapBounds = append(megaGapBounds, MinMax{Min: minGap, Max: maxGap})
				}

				requestInfos = append(requestInfos, requestInfo{
					traceID:      item.traceID,
					childTree:    childTree,
					childSpanID:  childSpanID,
					parentSpanID: item.spanID,
					parentStart:  item.startTime,
					parentDur:    item.duration,
				})
			}
		}

		if len(megaRequests) > 0 {
			megaResults := state.DependentAttributePredictor.SampleBatch(
				megaRequests, megaDurationBounds, megaGapBounds, rng,
			)

			for j, res := range megaResults {
				info := requestInfos[j]

				var gapFromParent, childDuration float64
				if state.Config.OffsetValue == "absolute" && state.Config.OffsetModel == "regression" {
					gapFromParent = math.Exp(clamp(res.GapRatio, 0, 30)) - 1
					childDuration = math.Exp(clamp(res.DurationRatio, 0, 30)) - 1
					gapFromParent = clamp(gapFromParent, 0, info.parentDur)
					childDuration = clamp(childDuration, 0, info.parentDur)
				} else {
					gapFromParent = res.GapRatio * info.parentDur
					childDuration = res.DurationRatio * info.parentDur
				}
				childStartTime := float64(info.parentStart) + gapFromParent

				if childStartTime < float64(info.parentStart) {
					childStartTime = float64(info.parentStart)
				}
				if childDuration < 1.0 {
					childDuration = 1.0
				}

				spanCount++
				if !countOnly {
					meta := IndicesToMetadata(res.MetadataIndices, state.DependentAttributes, state.DependentAttributeVocabs)

					allSpans = append(allSpans, GeneratedSpan{
						TraceID:      info.traceID,
						SpanID:       info.childSpanID,
						ParentSpanID: info.parentSpanID,
						NodeIdx:      info.childTree.Feature.NodeIdx,
						StartTime:    int64(childStartTime),
						Duration:     int64(childDuration),
						Metadata:     meta,
					})
				}

				nextLevel = append(nextLevel, levelItem{
					traceID:   info.traceID,
					treeNode:  info.childTree,
					spanID:    info.childSpanID,
					startTime: int64(childStartTime),
					duration:  childDuration,
				})
			}
		}

		currentLevel = nextLevel
	}

	return allSpans, spanCount
}
