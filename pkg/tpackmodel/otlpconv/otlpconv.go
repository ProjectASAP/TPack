// Package otlpconv adapts between OTLP pdata traces and the internal
// tpackmodel.Trace / GeneratedSpan types. It is the single owner of OTLP
// wire-format knowledge for tpackexporter, tpackreceiver, and tpack-eval.
//
// Rationale: ptrace is a wide, evolving API. Centralising the per-span
// extraction (service name, span kind, status code, attributes) and the
// reverse reconstruction means a bug or upstream rename only needs fixing
// once. Sub-package, not parent, keeps tpackmodel itself OTLP-free.
package otlpconv

import (
	"encoding/hex"
	"sort"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// SpanData is the OTLP-ready projection of one span. Callers (tpackreceiver
// from generated spans, tpack-eval from sampled or generated traces) populate
// these from their own input shapes and hand them to AppendSpans.
type SpanData struct {
	TraceID      string                  // 32-char hex
	SpanID       string                  // 16-char hex
	ParentSpanID string                  // 16-char hex; "" for roots
	Feature      tpackmodel.SpanFeature   // service.name comes from here
	StartTime    int64                   // microseconds
	Duration     int64                   // microseconds
	Metadata     map[string]string
}

// AppendSpans groups spans by Feature.ServiceName(), appends one ResourceSpans
// per service to td, and emits each span via ReconstructOTLP + metadata copy.
// Service names are emitted in sorted order so output is deterministic across
// callers and runs.
func AppendSpans(td ptrace.Traces, spans []SpanData) {
	byService := make(map[string][]SpanData)
	for _, sd := range spans {
		svc := sd.Feature.ServiceName()
		byService[svc] = append(byService[svc], sd)
	}

	services := make([]string, 0, len(byService))
	for svc := range byService {
		services = append(services, svc)
	}
	sort.Strings(services)

	for _, svc := range services {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", svc)
		ss := rs.ScopeSpans().AppendEmpty()
		for _, sd := range byService[svc] {
			s := ss.Spans().AppendEmpty()

			var tid pcommon.TraceID
			HexToBytes(sd.TraceID, tid[:])
			s.SetTraceID(tid)

			var sid pcommon.SpanID
			HexToBytes(sd.SpanID, sid[:])
			s.SetSpanID(sid)

			if sd.ParentSpanID != "" {
				var psid pcommon.SpanID
				HexToBytes(sd.ParentSpanID, psid[:])
				s.SetParentSpanID(psid)
			}

			ReconstructOTLP(sd.Feature, s)

			s.SetStartTimestamp(pcommon.Timestamp(sd.StartTime * 1000))
			s.SetEndTimestamp(pcommon.Timestamp((sd.StartTime + sd.Duration) * 1000))

			for k, v := range sd.Metadata {
				if v != "" {
					s.Attributes().PutStr(k, v)
				}
			}
		}
	}
}

// FromPdata flattens an OTLP trace batch into one *tpackmodel.Trace per
// trace ID. Spans for the same trace can arrive in any order across
// resource/scope groupings; this function merges them.
//
// primaryAttributes drive node identity (Dict). dependentAttributes are
// passthrough columns whose distributions the model preserves
// statistically — both are looked up via ExtractFeatureValues so callers
// get the same fallback behaviour regardless of which list a column lives
// in.
func FromPdata(td ptrace.Traces, primaryAttributes, dependentAttributes []string) []*tpackmodel.Trace {
	traceMap := make(map[string]*tpackmodel.Trace)

	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)

		serviceName := "unknown"
		if sn, ok := rs.Resource().Attributes().Get("service.name"); ok {
			serviceName = sn.Str()
		}

		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)

			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)

				tid := span.TraceID()
				traceID := hex.EncodeToString(tid[:])
				sid := span.SpanID()
				spanID := hex.EncodeToString(sid[:])
				parentSpanID := ""
				psid := span.ParentSpanID()
				if !psid.IsEmpty() {
					parentSpanID = hex.EncodeToString(psid[:])
				}

				featureValues := ExtractFeatureValues(primaryAttributes, serviceName, span)
				feature := tpackmodel.NewSpanFeature(primaryAttributes, featureValues)

				var meta map[string]string
				if len(dependentAttributes) > 0 {
					meta = ExtractFeatureValues(dependentAttributes, serviceName, span)
				}

				startTime := int64(span.StartTimestamp()) / 1000
				endTime := int64(span.EndTimestamp()) / 1000
				duration := max(endTime-startTime, 0)

				t, ok := traceMap[traceID]
				if !ok {
					t = &tpackmodel.Trace{TraceID: traceID, Spans: make(map[string]*tpackmodel.Span)}
					traceMap[traceID] = t
				}
				t.Spans[spanID] = &tpackmodel.Span{
					SpanID:       spanID,
					ParentSpanID: parentSpanID,
					Feature:      feature,
					StartTime:    startTime,
					Duration:     duration,
					Metadata:     meta,
				}
			}
		}
	}

	result := make([]*tpackmodel.Trace, 0, len(traceMap))
	for _, t := range traceMap {
		result = append(result, t)
	}
	return result
}

// ExtractFeatureValues reads the requested columns from the span. Well-known
// columns (service.name, span.kind, operation.name, status.code) are
// pulled from their first-class OTLP fields; everything else is read from
// span attributes via .AsString so non-string types (int, double, bool)
// round-trip correctly.
//
// span.kind and status.code fall back to span attributes when the OTLP
// field is empty/unset — this keeps round-trip parity when reading our
// own regenerated traces, where these values are written as attributes.
func ExtractFeatureValues(columns []string, serviceName string, span ptrace.Span) map[string]string {
	values := make(map[string]string, len(columns))
	for _, col := range columns {
		switch col {
		case "service.name":
			values[col] = serviceName
		case "span.kind":
			v := tpackmodel.SpanKindFromOTLP(int(span.Kind()))
			if v == "" || v == "UNSPECIFIED" {
				if attr, ok := span.Attributes().Get(col); ok {
					v = attr.AsString()
				}
			}
			values[col] = v
		case "operation.name":
			values[col] = span.Name()
		case "status.code":
			v := tpackmodel.StatusCodeFromOTLP(int(span.Status().Code()))
			if v == "" || v == "0" {
				if attr, ok := span.Attributes().Get(col); ok {
					v = attr.AsString()
				}
			}
			values[col] = v
		default:
			if v, ok := span.Attributes().Get(col); ok {
				values[col] = v.AsString()
			}
		}
	}
	return values
}

// ReconstructOTLP writes feature values back onto an OTLP span. service.name
// is intentionally skipped: callers group by service and set it on the
// ResourceSpans' resource attributes instead.
func ReconstructOTLP(feat tpackmodel.SpanFeature, span ptrace.Span) {
	values := feat.Values()
	for k, v := range values {
		switch k {
		case "service.name":
			// Already on the resource by caller convention.
		case "span.kind":
			span.SetKind(ptrace.SpanKind(tpackmodel.SpanKindToOTLP(v)))
		case "operation.name":
			if v != "" {
				span.SetName(v)
			} else {
				span.SetName(values["service.name"])
			}
		case "status.code":
			switch v {
			case "2":
				span.Status().SetCode(ptrace.StatusCodeError)
			case "1":
				span.Status().SetCode(ptrace.StatusCodeOk)
			default:
				span.Status().SetCode(ptrace.StatusCodeUnset)
			}
		default:
			if v != "" {
				span.Attributes().PutStr(k, v)
			}
		}
	}
	if _, hasOpName := values["operation.name"]; !hasOpName {
		if svc := values["service.name"]; svc != "" {
			span.SetName(svc)
		}
	}
}

// HexToBytes decodes a hex string into dst. Unknown hex characters decode
// to 0; callers using fixed-width IDs (16-char span ID, 32-char trace ID)
// should size dst accordingly.
func HexToBytes(s string, dst []byte) {
	for i := 0; i < len(dst) && i*2+1 < len(s); i++ {
		dst[i] = hexNibble(s[i*2])<<4 | hexNibble(s[i*2+1])
	}
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}
