package main

import (
	"runtime"
	"sort"
	"sync"
)

// spanNodeKey returns the graph node key for a span: "service.name/operation.name".
func spanNodeKey(span *evalSpan) string {
	svc := span.Feature.ServiceName()
	op := span.Feature.OperationName()
	if op == "" {
		return svc
	}
	return svc + "/" + op
}

// evaluateGraph builds per-minute graphs with edge call counts.
// Nodes are keyed by service.name/operation.name.
// Output: {"service_graph_by_time": {"bucket": {"nodes": [...], "edges": {"a->b": n}}}}
func evaluateGraph(dir string, traces []evalTrace) error {
	type graphData struct {
		nodes map[string]struct{}
		edges map[string]int
	}

	// Parallel shard: each worker builds a local graphByTime, then merge.
	nw := max(min(runtime.NumCPU(), len(traces)), 1)
	partials := make([]map[int64]*graphData, nw)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make(map[int64]*graphData)
			for i := w; i < len(traces); i += nw {
				trace := traces[i]
				for _, span := range trace {
					node := spanNodeKey(span)
					bucket := timeBucketMinute(span.StartTime)

					gd, ok := local[bucket]
					if !ok {
						gd = &graphData{
							nodes: make(map[string]struct{}),
							edges: make(map[string]int),
						}
						local[bucket] = gd
					}

					gd.nodes[node] = struct{}{}

					if span.ParentSpanID != "" {
						if parent, ok := trace[span.ParentSpanID]; ok {
							parentNode := spanNodeKey(parent)
							edgeKey := parentNode + "->" + node
							gd.edges[edgeKey]++
						}
					}
				}
			}
			partials[w] = local
		}(w)
	}
	wg.Wait()

	// Merge per-bucket graph data
	graphByTime := make(map[int64]*graphData)
	for _, local := range partials {
		for bucket, gd := range local {
			merged, ok := graphByTime[bucket]
			if !ok {
				merged = &graphData{
					nodes: make(map[string]struct{}),
					edges: make(map[string]int),
				}
				graphByTime[bucket] = merged
			}
			for n := range gd.nodes {
				merged.nodes[n] = struct{}{}
			}
			for e, c := range gd.edges {
				merged.edges[e] += c
			}
		}
	}

	// Convert to output format (int64 map keys become JSON string keys)
	resultMap := make(map[int64]any, len(graphByTime))
	for bucket, gd := range graphByTime {
		nodes := make([]string, 0, len(gd.nodes))
		for n := range gd.nodes {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)

		resultMap[bucket] = map[string]any{
			"nodes": nodes,
			"edges": gd.edges,
		}
	}

	result := map[string]any{
		"service_graph_by_time": resultMap,
	}

	return writeEvalResult(dir, "graph_results.json", result)
}
