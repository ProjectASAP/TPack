package tpackexporter

import (
	"maps"
	"sync"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// traceBuffer accumulates traces with time-bucket awareness.
// It merges spans by traceID so that cross-service spans from separate
// ConsumeTraces calls are combined into a single tpackmodel.Trace entry.
// It triggers flushing after a configurable interval or when the max count is reached.
type traceBuffer struct {
	mu          sync.Mutex
	tracesByID  map[string]*tpackmodel.Trace
	maxTraces   int
	lastReceive time.Time
}

func newTraceBuffer(maxTraces int) *traceBuffer {
	return &traceBuffer{
		tracesByID: make(map[string]*tpackmodel.Trace),
		maxTraces:  maxTraces,
	}
}

// add merges traces into the buffer by traceID. Returns true if the buffer is full.
func (b *traceBuffer) add(traces []*tpackmodel.Trace) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, t := range traces {
		if existing, ok := b.tracesByID[t.TraceID]; ok {
			maps.Copy(existing.Spans, t.Spans)
		} else {
			b.tracesByID[t.TraceID] = t
		}
	}
	b.lastReceive = time.Now()

	return len(b.tracesByID) >= b.maxTraces
}

// flush returns all buffered traces and resets the buffer.
func (b *traceBuffer) flush() []*tpackmodel.Trace {
	b.mu.Lock()
	defer b.mu.Unlock()

	traces := make([]*tpackmodel.Trace, 0, len(b.tracesByID))
	for _, t := range b.tracesByID {
		traces = append(traces, t)
	}
	b.tracesByID = make(map[string]*tpackmodel.Trace)
	return traces
}

// size returns the current number of buffered traces.
func (b *traceBuffer) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.tracesByID)
}

// timeSinceLastReceive returns how long since the last trace was received.
func (b *traceBuffer) timeSinceLastReceive() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.lastReceive.IsZero() {
		return 0
	}
	return time.Since(b.lastReceive)
}
