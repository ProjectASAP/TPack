package tpackmodel

import (
	"sort"
	"strings"
)

// SpanFeature is a canonical string encoding of key-value pairs that determine
// node identity. Format: "k1=v1\x00k2=v2\x00..." with keys sorted alphabetically.
// Since it's a string, it's directly usable as a Go map key.
type SpanFeature string

const pairSep = "\x00" // separator between key=value pairs
const kvSep = "="      // separator between key and value

// DefaultFeatureColumns is the default set of columns that form node identity.
var DefaultFeatureColumns = []string{"service.name", "span.kind", "operation.name", "status.code"}

// WellKnownColumns lists columns with special OTLP extraction/reconstruction logic.
// Any column not in this set is treated as a span attribute.
var WellKnownColumns = map[string]bool{
	"service.name":   true,
	"span.kind":      true,
	"operation.name": true,
	"status.code":    true,
}

// NewSpanFeature builds a SpanFeature from the given columns and values.
// Only the specified columns are included, sorted alphabetically.
// Missing columns get empty string values.
func NewSpanFeature(columns []string, values map[string]string) SpanFeature {
	sorted := make([]string, len(columns))
	copy(sorted, columns)
	sort.Strings(sorted)

	var b strings.Builder
	for i, k := range sorted {
		if i > 0 {
			b.WriteString(pairSep)
		}
		b.WriteString(k)
		b.WriteString(kvSep)
		b.WriteString(values[k])
	}
	return SpanFeature(b.String())
}

// Get returns the value for a given key, or "" if not present.
func (f SpanFeature) Get(key string) string {
	s := string(f)
	// Linear scan through pairs (typically 4-6 columns, no need for binary search)
	for s != "" {
		var pair string
		if idx := strings.Index(s, pairSep); idx >= 0 {
			pair = s[:idx]
			s = s[idx+1:]
		} else {
			pair = s
			s = ""
		}
		if before, after, ok := strings.Cut(pair, kvSep); ok {
			if before == key {
				return after
			}
		}
	}
	return ""
}

// Range calls fn for each (key, value) pair. Stops early if fn returns false.
// Does not allocate a map, unlike Values().
func (f SpanFeature) Range(fn func(key, val string) bool) {
	s := string(f)
	for s != "" {
		var pair string
		if idx := strings.Index(s, pairSep); idx >= 0 {
			pair = s[:idx]
			s = s[idx+1:]
		} else {
			pair = s
			s = ""
		}
		if before, after, ok := strings.Cut(pair, kvSep); ok {
			if !fn(before, after) {
				return
			}
		}
	}
}

// Values parses all key-value pairs and returns them as a map.
func (f SpanFeature) Values() map[string]string {
	s := string(f)
	if s == "" {
		return nil
	}
	result := make(map[string]string)
	for s != "" {
		var pair string
		if idx := strings.Index(s, pairSep); idx >= 0 {
			pair = s[:idx]
			s = s[idx+1:]
		} else {
			pair = s
			s = ""
		}
		if before, after, ok := strings.Cut(pair, kvSep); ok {
			result[before] = after
		}
	}
	return result
}

// String returns the canonical form (same as the underlying string).
func (f SpanFeature) String() string {
	return string(f)
}

// Convenience accessors for common well-known columns.

func (f SpanFeature) ServiceName() string   { return f.Get("service.name") }
func (f SpanFeature) SpanKind() string      { return f.Get("span.kind") }
func (f SpanFeature) OperationName() string { return f.Get("operation.name") }
func (f SpanFeature) StatusCode() string    { return f.Get("status.code") }

// IsError returns true if the span has an error status code.
func (f SpanFeature) IsError() bool {
	return f.Get("status.code") == "2"
}

// ParseSpanFeature converts a serialized string back to a SpanFeature.
// The string IS the canonical form, so this is a simple cast.
func ParseSpanFeature(s string) SpanFeature {
	return SpanFeature(s)
}

// StatusCodeFromOTLP converts an OTel status code integer to the string format.
// 0=Unset->"0", 1=Ok->"1", 2=Error->"2".
func StatusCodeFromOTLP(code int) string {
	switch code {
	case 2:
		return "2"
	case 1:
		return "1"
	default:
		return "0"
	}
}

// SpanKindFromOTLP converts an OTel span kind integer to the string format.
func SpanKindFromOTLP(kind int) string {
	switch kind {
	case 1:
		return "INTERNAL"
	case 2:
		return "SERVER"
	case 3:
		return "CLIENT"
	case 4:
		return "PRODUCER"
	case 5:
		return "CONSUMER"
	default:
		return ""
	}
}

// SpanKindToOTLP converts a span kind string back to OTel integer.
func SpanKindToOTLP(s string) int {
	switch s {
	case "INTERNAL":
		return 1
	case "SERVER":
		return 2
	case "CLIENT":
		return 3
	case "PRODUCER":
		return 4
	case "CONSUMER":
		return 5
	default:
		return 0
	}
}
