package main

import (
	"fmt"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func testJSON() {
	td := ptrace.NewTraces()
	
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "frontend")
	
	ss := rs.ScopeSpans().AppendEmpty()
	
	span := ss.Spans().AppendEmpty()
	span.SetTraceID(pcommon.TraceID([16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}))
	span.SetSpanID(pcommon.SpanID([8]byte{0, 0, 0, 0, 0, 0, 0, 1}))
	span.SetName("GET /api")
	span.SetKind(ptrace.SpanKindServer)
	span.Attributes().PutStr("http.method", "GET")
	span.Attributes().PutStr("http.status_code", "200")
	span.SetStartTimestamp(pcommon.Timestamp(1000000000000))
	span.SetEndTimestamp(pcommon.Timestamp(1000005000000))
	
	marshaler := &ptrace.JSONMarshaler{}
	data, err := marshaler.MarshalTraces(td)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	
	fmt.Println(string(data))
}
