package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// writeFlattenedCSV reads OTLP traces and writes a flat CSV with one row per span.
// Computes tree metadata (depth, parent service/operation, siblings, minute bucket).
// Extra columns: non-core feature columns + metadata columns are appended as additional CSV columns.
func writeFlattenedCSV(outputPath string, buckets map[int64][]*tpackmodel.Trace, primaryAttributes, dependentAttributes []string) error {
	t0 := time.Now()

	// Determine extra columns: non-core feature columns + all metadata columns
	coreFeatures := map[string]bool{
		"service.name": true, "operation.name": true, "span.kind": true, "status.code": true,
	}
	var extraColumns []string
	for _, col := range primaryAttributes {
		if !coreFeatures[col] {
			extraColumns = append(extraColumns, col)
		}
	}
	extraColumns = append(extraColumns, dependentAttributes...)

	// Collect all spans with trace context
	type flatSpan struct {
		traceID          string
		spanID           string
		parentSpanID     string
		serviceName      string
		operationName    string
		spanKind         string
		statusCode       string
		startTimeUs      int64
		durationUs       int64
		depth            int
		parentService    string
		parentOperation  string
		numSiblings      int
		minuteBucket     int64
		extraValues      []string // one value per extraColumns entry
	}

	var rows []flatSpan
	totalSpans := 0

	for bucketKey, traces := range buckets {
		for _, td := range traces {
			// Find trace min start time for offset computation
			minStart := int64(math.MaxInt64)
			for _, s := range td.Spans {
				if s.StartTime < minStart {
					minStart = s.StartTime
				}
			}
			minuteBucket := bucketKey

			// Build children map for sibling count
			childrenOf := make(map[string][]string) // parentSpanID -> []childSpanID
			for spanID, s := range td.Spans {
				if s.ParentSpanID != "" {
					if _, ok := td.Spans[s.ParentSpanID]; ok {
						childrenOf[s.ParentSpanID] = append(childrenOf[s.ParentSpanID], spanID)
					}
				}
			}

			// Compute depth with memoization
			depthCache := make(map[string]int)
			var getDepth func(spanID string) int
			getDepth = func(spanID string) int {
				if d, ok := depthCache[spanID]; ok {
					return d
				}
				s := td.Spans[spanID]
				if s == nil || s.ParentSpanID == "" {
					depthCache[spanID] = 0
					return 0
				}
				if _, ok := td.Spans[s.ParentSpanID]; !ok {
					depthCache[spanID] = 0
					return 0
				}
				d := 1 + getDepth(s.ParentSpanID)
				depthCache[spanID] = d
				return d
			}

			for spanID, s := range td.Spans {
				depth := getDepth(spanID)

				parentSvc := ""
				parentOp := ""
				if s.ParentSpanID != "" {
					if parent, ok := td.Spans[s.ParentSpanID]; ok {
						parentSvc = parent.Feature.ServiceName()
						parentOp = parent.Feature.OperationName()
					}
				}

				numSiblings := 0
				if s.ParentSpanID != "" {
					numSiblings = len(childrenOf[s.ParentSpanID])
				}

				// Collect extra column values from features and metadata
				extras := make([]string, len(extraColumns))
				featVals := s.Feature.Values()
				for i, col := range extraColumns {
					if v, ok := featVals[col]; ok && v != "" {
						extras[i] = v
					} else if s.Metadata != nil {
						extras[i] = s.Metadata[col]
					}
				}

				rows = append(rows, flatSpan{
					traceID:         td.TraceID,
					spanID:          spanID,
					parentSpanID:    s.ParentSpanID,
					serviceName:     s.Feature.ServiceName(),
					operationName:   s.Feature.OperationName(),
					spanKind:        s.Feature.SpanKind(),
					statusCode:      s.Feature.StatusCode(),
					startTimeUs:     s.StartTime,
					durationUs:      s.Duration,
					depth:           depth,
					parentService:   parentSvc,
					parentOperation: parentOp,
					numSiblings:     numSiblings,
					minuteBucket:    minuteBucket,
					extraValues:     extras,
				})
				totalSpans++
			}
		}
	}

	log.Printf("Flatten CSV: %d spans collected in %v", totalSpans, time.Since(t0))

	// Write CSV
	t1 := time.Now()
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outputPath, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)

	// Header
	header := []string{
		"trace_id", "span_id", "parent_span_id",
		"service_name", "operation_name", "span_kind", "status_code",
		"start_time_us", "duration_us",
		"depth", "parent_service_name", "parent_operation_name",
		"num_siblings", "minute_bucket",
	}
	header = append(header, extraColumns...)
	if err := w.Write(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	for _, r := range rows {
		record := []string{
			r.traceID,
			r.spanID,
			r.parentSpanID,
			r.serviceName,
			r.operationName,
			r.spanKind,
			r.statusCode,
			strconv.FormatInt(r.startTimeUs, 10),
			strconv.FormatInt(r.durationUs, 10),
			strconv.Itoa(r.depth),
			r.parentService,
			r.parentOperation,
			strconv.Itoa(r.numSiblings),
			strconv.FormatInt(r.minuteBucket, 10),
		}
		record = append(record, r.extraValues...)
		if err := w.Write(record); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("csv flush: %w", err)
	}

	fi, _ := os.Stat(outputPath)
	log.Printf("Flatten CSV: wrote %s (%d MB) in %v",
		outputPath, fi.Size()/(1024*1024), time.Since(t1))

	return nil
}
