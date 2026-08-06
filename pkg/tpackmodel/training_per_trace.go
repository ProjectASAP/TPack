package tpackmodel

import (
	"math"
	"sort"
)

// trainTopologyFromTrace updates the topology model from a single indexed trace.
// Honors TPackConfig.TopologyMode ("edge" or "template").
func trainTopologyFromTrace(
	state *TPackModelState,
	t indexedTrace,
	traceType TraceType,
	childCounts map[string]int32,
	maxTreeSize *int32,
) {
	parentChildren := make(map[string][]*indexedSpan)
	for _, s := range t.Spans {
		if s.ParentSpanID != "" {
			if _, ok := t.Spans[s.ParentSpanID]; ok {
				parentChildren[s.ParentSpanID] = append(parentChildren[s.ParentSpanID], s)
			}
		}
	}

	for _, children := range parentChildren {
		sort.Slice(children, func(i, j int) bool {
			return children[i].StartTime < children[j].StartTime
		})
	}

	for spanID, s := range t.Spans {
		_, parentExists := t.Spans[s.ParentSpanID]
		if s.ParentSpanID == "" || !parentExists {
			treeSize := countTreeSize(spanID, parentChildren)
			if treeSize > *maxTreeSize {
				*maxTreeSize = treeSize
			}
		}
	}

	if state.Config.TopologyMode == "template" {
		// Template mode: store full tree topologies.
		for spanID, s := range t.Spans {
			_, parentExists := t.Spans[s.ParentSpanID]
			if s.ParentSpanID == "" || !parentExists {
				nodes, parentIdxs := extractTraceTemplate(spanID, t.Spans, parentChildren)
				state.TopologyModel.AddTemplate(traceType, nodes, parentIdxs)
			}
		}
		return
	}

	// Edge mode: store per-edge counts.
	for parentID, children := range parentChildren {
		parentSpan := t.Spans[parentID]
		parentFeature := NodeFeature{
			NodeIdx:    parentSpan.NodeIdx,
			ChildIdx:   -1,
			ChildCount: int32(len(children)),
		}
		for childIdx, child := range children {
			grandChildCount := childCounts[child.SpanID]
			pos := int32(childIdx)
			childFeature := NodeFeature{
				NodeIdx:    child.NodeIdx,
				ChildIdx:   pos,
				ChildCount: grandChildCount,
			}
			state.TopologyModel.AddEdge(traceType, parentFeature, pos, childFeature, 1)
		}
	}
}

// extractTraceTemplate does a BFS from rootSpanID and returns a template
// (BFS-ordered nodes with parent indices). Used by template topology mode.
func extractTraceTemplate(
	rootSpanID string,
	spans map[string]*indexedSpan,
	parentChildren map[string][]*indexedSpan,
) ([]NodeFeature, []int32) {
	var nodes []NodeFeature
	var parentIndices []int32
	bfsIdx := map[string]int32{}

	queue := []string{rootSpanID}
	for len(queue) > 0 {
		spanID := queue[0]
		queue = queue[1:]
		span := spans[spanID]
		children := parentChildren[spanID]

		idx := int32(len(nodes))
		bfsIdx[spanID] = idx

		parentIdx := int32(-1)
		if span.ParentSpanID != "" {
			if pidx, ok := bfsIdx[span.ParentSpanID]; ok {
				parentIdx = pidx
			}
		}

		nodes = append(nodes, NodeFeature{
			NodeIdx:    span.NodeIdx,
			ChildIdx:   -1,
			ChildCount: int32(len(children)),
		})
		parentIndices = append(parentIndices, parentIdx)

		for _, child := range children {
			queue = append(queue, child.SpanID)
		}
	}
	return nodes, parentIndices
}

// trainBoundsFromTrace updates the per-node duration and gap bounds.
func trainBoundsFromTrace(state *TPackModelState, t indexedTrace) {
	for _, s := range t.Spans {
		state.SpanDurationBounds.Update(s.NodeIdx, float64(s.Duration))

		if s.ParentSpanID != "" {
			if parent, ok := t.Spans[s.ParentSpanID]; ok {
				gap := float64(s.StartTime - parent.StartTime)
				state.SpanGapBounds.Update(s.NodeIdx, gap)
			}
		}
	}
}

// trainMetadataFromTrace updates the metadata predictor from a single indexed trace.
// Honors TPackConfig.OffsetValue ("ratio" or "absolute") and OffsetModel
// ("regression" or "percentile") for child gap/duration targets. Roots are
// trained with a NO_PARENT sentinel so generation can sample their metadata.
func trainMetadataFromTrace(
	state *TPackModelState,
	t indexedTrace,
	dependentAttributes []string,
) {
	sp, ok := state.DependentAttributePredictor.(*StatisticalDependentAttributePredictor)
	if !ok {
		return
	}
	addSample := sp.AddSample

	parentChildren := make(map[string][]*indexedSpan)
	for _, s := range t.Spans {
		if s.ParentSpanID != "" {
			if _, ok := t.Spans[s.ParentSpanID]; ok {
				parentChildren[s.ParentSpanID] = append(parentChildren[s.ParentSpanID], s)
			}
		}
	}
	for _, children := range parentChildren {
		sort.Slice(children, func(i, j int) bool {
			return children[i].StartTime < children[j].StartTime
		})
	}

	// Train root spans with NO_PARENT sentinel (ParentFeature.NodeIdx = vocabSize).
	noParentIdx := int32(state.NodeEncoder.VocabSize())
	for _, s := range t.Spans {
		isRoot := s.ParentSpanID == ""
		if !isRoot {
			if _, ok := t.Spans[s.ParentSpanID]; !ok {
				isRoot = true // parent not in this trace
			}
		}
		if !isRoot {
			continue
		}

		metaIndices := make([]int, len(dependentAttributes))
		for colIdx, col := range dependentAttributes {
			val := ""
			if s.Metadata != nil {
				val = s.Metadata[col]
			}
			if vocab, ok := state.DependentAttributeVocabs[col]; ok {
				for i, v := range vocab {
					if v == val {
						metaIndices[colIdx] = i
						break
					}
				}
			}
		}

		dur := float64(s.Duration)
		if dur <= 0 {
			dur = 1
		}
		addSample(noParentIdx, s.NodeIdx, 0, 1, dur, 0, metaIndices)
	}

	for parentID, children := range parentChildren {
		parent := t.Spans[parentID]
		if parent.Duration <= 0 {
			continue
		}
		childCount := len(children)

		for childIdx, child := range children {
			gap := float64(child.StartTime - parent.StartTime)
			var gapTarget, durTarget float64
			if state.Config.OffsetValue == "absolute" {
				if state.Config.OffsetModel == "regression" {
					// Log-transform for regression in absolute space.
					gapTarget = math.Log(math.Max(0, gap) + 1)
					durTarget = math.Log(math.Max(0, float64(child.Duration)) + 1)
				} else {
					// Raw absolute µs for percentile.
					gapTarget = math.Max(0, gap)
					durTarget = math.Max(0, float64(child.Duration))
				}
			} else {
				// Ratio space (works for both regression and percentile).
				gapTarget = clamp(gap/float64(parent.Duration), 0, 1)
				durTarget = clamp(float64(child.Duration)/float64(parent.Duration), 0, 1)
			}
			gapRatio := gapTarget
			durRatio := durTarget

			normalizedIdx := 0.0
			if childCount > 1 {
				normalizedIdx = float64(childIdx) / float64(childCount-1)
			}

			metaIndices := make([]int, len(dependentAttributes))
			for colIdx, col := range dependentAttributes {
				val := ""
				if child.Metadata != nil {
					val = child.Metadata[col]
				}
				if vocab, ok := state.DependentAttributeVocabs[col]; ok {
					for i, v := range vocab {
						if v == val {
							metaIndices[colIdx] = i
							break
						}
					}
				}
			}

			addSample(parent.NodeIdx, child.NodeIdx, gapRatio, durRatio, float64(parent.Duration), normalizedIdx, metaIndices)
		}
	}
}

// finalizeMetadataPredictor finalizes the predictor's sufficient statistics.
func finalizeMetadataPredictor(state *TPackModelState) {
	if p, ok := state.DependentAttributePredictor.(*StatisticalDependentAttributePredictor); ok {
		p.FinalizeFit()
	}
}
