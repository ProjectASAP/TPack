package main

import (
	"sort"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// extractPaths extracts all N-length paths from a trace tree via DFS.
// Each path is a sequence of N span type indices following parent-child relationships.
// Traces with max depth < N yield zero paths.
func extractPaths(t *tpackmodel.Trace, encoder *tpackmodel.NodeEncoder, N int) [][5]int32 {
	// Build parent -> children map sorted by start time
	parentChildren := make(map[string][]string)
	for _, s := range t.Spans {
		if s.ParentSpanID != "" {
			if _, ok := t.Spans[s.ParentSpanID]; ok {
				parentChildren[s.ParentSpanID] = append(parentChildren[s.ParentSpanID], s.SpanID)
			}
		}
	}
	for pid := range parentChildren {
		children := parentChildren[pid]
		sort.Slice(children, func(i, j int) bool {
			return t.Spans[children[i]].StartTime < t.Spans[children[j]].StartTime
		})
	}

	// Find root spans
	var roots []string
	for _, s := range t.Spans {
		_, parentExists := t.Spans[s.ParentSpanID]
		if s.ParentSpanID == "" || !parentExists {
			roots = append(roots, s.SpanID)
		}
	}

	var paths [][5]int32
	chain := make([]int32, 0, 16)
	for _, root := range roots {
		dfsCollectPaths(root, t, parentChildren, encoder, chain, N, &paths)
	}
	return paths
}

// dfsCollectPaths performs DFS, maintaining the ancestor chain and extracting
// length-N sliding windows at every node with sufficient depth.
func dfsCollectPaths(
	spanID string,
	t *tpackmodel.Trace,
	parentChildren map[string][]string,
	encoder *tpackmodel.NodeEncoder,
	chain []int32,
	N int,
	result *[][5]int32,
) {
	nodeIdx := encoder.Transform(t.Spans[spanID].Feature)

	// Extend chain (copy to avoid aliasing across branches)
	newChain := make([]int32, len(chain)+1)
	copy(newChain, chain)
	newChain[len(chain)] = nodeIdx

	children := parentChildren[spanID]

	if len(children) == 0 {
		// Leaf: extract all length-N windows from this root-to-leaf chain
		extractWindows(newChain, N, result)
	} else {
		// Internal node: also extract windows (paths don't have to end at leaves)
		extractWindows(newChain, N, result)
		for _, child := range children {
			dfsCollectPaths(child, t, parentChildren, encoder, newChain, N, result)
		}
	}
}

// extractWindows extracts all contiguous windows of length N from a chain.
// Only extracts windows that haven't been extracted by a shorter prefix of the same chain.
func extractWindows(chain []int32, N int, result *[][5]int32) {
	if len(chain) < N {
		return
	}
	// Only extract the window that ends at the current position (last element of chain).
	// This avoids duplicates: earlier windows were already extracted when the chain was shorter.
	i := len(chain) - N
	var path [5]int32
	copy(path[:], chain[i:i+N])
	*result = append(*result, path)
}

// pathContextAndTarget splits a 5-element path into context (indices 0,1,3,4) and target (index 2).
// This implements the "predict middle from context" formulation from the Sifter paper.
func pathContextAndTarget(path [5]int32) ([4]int32, int32) {
	context := [4]int32{path[0], path[1], path[3], path[4]}
	target := path[2]
	return context, target
}
