package main

import (
	"runtime"
	"sync"
)

// evaluateSpanCount computes tree sizes per trace using Union-Find.
// Output: {"span_count": {"all": [sizes...]}}
func evaluateSpanCount(dir string, traces []evalTrace) error {
	// Parallel: each worker processes a shard, builds a local []int of sizes.
	nw := max(min(runtime.NumCPU(), len(traces)), 1)
	partials := make([][]int, nw)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var local []int
			for i := w; i < len(traces); i += nw {
				trace := traces[i]
				if len(trace) == 0 {
					continue
				}

				// Map span IDs to indices
				spanIDs := make([]string, 0, len(trace))
				idToIdx := make(map[string]int, len(trace))
				for spanID := range trace {
					idToIdx[spanID] = len(spanIDs)
					spanIDs = append(spanIDs, spanID)
				}

				uf := newUnionFind(len(spanIDs))
				for spanID, span := range trace {
					if span.ParentSpanID != "" {
						if parentIdx, ok := idToIdx[span.ParentSpanID]; ok {
							uf.union(idToIdx[spanID], parentIdx)
						}
					}
				}

				local = append(local, uf.componentSizes()...)
			}
			partials[w] = local
		}(w)
	}
	wg.Wait()

	// Concat all partials
	total := 0
	for _, p := range partials {
		total += len(p)
	}
	allSizes := make([]int, 0, total)
	for _, p := range partials {
		allSizes = append(allSizes, p...)
	}

	result := map[string]any{
		"span_count": map[string]any{
			"all": allSizes,
		},
	}

	return writeEvalResult(dir, "span_count_results.json", result)
}

// unionFind implements Union-Find with path compression and union by rank.
type unionFind struct {
	parent []int
	size   []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{
		parent: make([]int, n),
		size:   make([]int, n),
		rank:   make([]int, n),
	}
	for i := range uf.parent {
		uf.parent[i] = i
		uf.size[i] = 1
	}
	return uf
}

func (uf *unionFind) find(x int) int {
	if uf.parent[x] != x {
		uf.parent[x] = uf.find(uf.parent[x])
	}
	return uf.parent[x]
}

func (uf *unionFind) union(x, y int) {
	rootX, rootY := uf.find(x), uf.find(y)
	if rootX == rootY {
		return
	}
	if uf.rank[rootX] < uf.rank[rootY] {
		rootX, rootY = rootY, rootX
	}
	uf.parent[rootY] = rootX
	uf.size[rootX] += uf.size[rootY]
	if uf.rank[rootX] == uf.rank[rootY] {
		uf.rank[rootX]++
	}
}

func (uf *unionFind) componentSizes() []int {
	seen := make(map[int]struct{})
	var sizes []int
	for i := range uf.parent {
		root := uf.find(i)
		if _, ok := seen[root]; !ok {
			seen[root] = struct{}{}
			sizes = append(sizes, uf.size[root])
		}
	}
	return sizes
}
