package tpackmodel

import "math"

// indexedSpan is a Span with NodeIdx replacing the SpanFeature string.
// Internal training intermediate; not exported.
type indexedSpan struct {
	SpanID       string
	ParentSpanID string
	NodeIdx      int32
	StartTime    int64
	Duration     int64
	Metadata     map[string]string
}

// indexedTrace is a Trace with all spans indexed.
type indexedTrace struct {
	Spans     map[string]*indexedSpan
	StartTime int64
	IsError   bool
}

// indexTrace converts a Trace into an indexedTrace using the given encoder.
// Returns the indexed trace and its earliest span StartTime (used to track
// the global timestamp range across a bucket).
func indexTrace(t *Trace, encoder *NodeEncoder) (indexedTrace, int64) {
	it := indexedTrace{
		Spans:   make(map[string]*indexedSpan, len(t.Spans)),
		IsError: IsErrorTrace(t),
	}
	minStart := int64(math.MaxInt64)
	for spanID, s := range t.Spans {
		it.Spans[spanID] = &indexedSpan{
			SpanID:       s.SpanID,
			ParentSpanID: s.ParentSpanID,
			NodeIdx:      encoder.Transform(s.Feature),
			StartTime:    s.StartTime,
			Duration:     s.Duration,
			Metadata:     s.Metadata,
		}
		if s.StartTime < minStart {
			minStart = s.StartTime
		}
	}
	it.StartTime = minStart
	return it, minStart
}

// childCountsOf returns parentSpanID → number of direct children.
func childCountsOf(t indexedTrace) map[string]int32 {
	childCounts := make(map[string]int32)
	for _, s := range t.Spans {
		if s.ParentSpanID != "" {
			childCounts[s.ParentSpanID]++
		}
	}
	return childCounts
}

// countTreeSize counts nodes reachable from rootID in the parent→children map.
func countTreeSize(rootID string, parentChildren map[string][]*indexedSpan) int32 {
	count := int32(1)
	for _, child := range parentChildren[rootID] {
		count += countTreeSize(child.SpanID, parentChildren)
	}
	return count
}
