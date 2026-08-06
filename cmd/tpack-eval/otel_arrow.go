package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	arrowpb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	cfg "github.com/open-telemetry/otel-arrow/go/pkg/config"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"google.golang.org/protobuf/proto"
)

// OtelArrowResult holds the results of OTel-Arrow encode/decode benchmarking.
type OtelArrowResult struct {
	EncodedSize   int64
	EncodeSeconds float64
	DecodeSeconds float64
}

// maxSpansPerArrowBatch caps each BatchArrowRecords at a span count where
// the library's uint16 Attributes16Accumulator won't overflow in practice.
// Upstream otelarrowexporter's batch_processor default is 8192 spans; we go
// a bit lower to be safe against high-attribute-cardinality workloads.
const maxSpansPerArrowBatch = 5000

// benchmarkOtelArrow reads *.pb / *.json chunk files from dir, encodes each
// as Apache Arrow IPC via arrow_record.Producer (→ BatchArrowRecords →
// proto.Marshal; the same wire format the upstream otelarrowexporter sends),
// then decodes via arrow_record.Consumer. Encode/decode wall time is measured
// separately from disk I/O. When writeDir is non-empty, encoded bytes are
// persisted as model_bucket_{i}_{j} so report.go can pick up the transmission
// size by walking the directory (same convention as gzip).
//
// Each chunk is split into sub-batches of at most maxSpansPerArrowBatch spans
// to avoid overflowing the library's internal uint16-indexed attribute
// dictionary. Each sub-batch is encoded with a fresh Producer (independent
// dictionary state) — matches what the upstream batch_processor does on the
// wire.
func benchmarkOtelArrow(dir string, decompress bool, writeDir string) (OtelArrowResult, error) {
	jsonFiles, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	pbFiles, _ := filepath.Glob(filepath.Join(dir, "*.pb"))
	files := append(jsonFiles, pbFiles...)
	if len(files) == 0 {
		return OtelArrowResult{}, fmt.Errorf("no .json or .pb files in %s", dir)
	}
	sort.Strings(files)

	if writeDir != "" {
		if err := os.MkdirAll(writeDir, 0o755); err != nil {
			return OtelArrowResult{}, fmt.Errorf("mkdir %s: %w", writeDir, err)
		}
	}

	fmt.Fprintf(os.Stderr, "  otel-arrow benchmark: %d files\n", len(files))

	var totalSize int64
	var totalEncSecs, totalDecSecs float64

	for i, file := range files {
		td, err := readOTLPFile(file)
		if err != nil {
			return OtelArrowResult{}, fmt.Errorf("read %s: %w", file, err)
		}

		encStart := time.Now()
		subBatches, err := encodeArrowSubBatches(td, maxSpansPerArrowBatch)
		if err != nil {
			return OtelArrowResult{}, fmt.Errorf("encode %s: %w", file, err)
		}
		totalEncSecs += time.Since(encStart).Seconds()

		for j, encoded := range subBatches {
			totalSize += int64(len(encoded))
			if writeDir != "" {
				path := filepath.Join(writeDir, fmt.Sprintf("model_bucket_%d_%d", i, j))
				if err := os.WriteFile(path, encoded, 0o644); err != nil {
					return OtelArrowResult{}, fmt.Errorf("write %s: %w", path, err)
				}
			}
		}

		if decompress {
			decStart := time.Now()
			for _, encoded := range subBatches {
				var decoded arrowpb.BatchArrowRecords
				if err := proto.Unmarshal(encoded, &decoded); err != nil {
					return OtelArrowResult{}, fmt.Errorf("unmarshal %s: %w", file, err)
				}
				consumer := arrow_record.NewConsumer()
				if _, err := consumer.TracesFrom(&decoded); err != nil {
					consumer.Close()
					return OtelArrowResult{}, fmt.Errorf("decode %s: %w", file, err)
				}
				if err := consumer.Close(); err != nil {
					return OtelArrowResult{}, fmt.Errorf("close consumer for %s: %w", file, err)
				}
			}
			totalDecSecs += time.Since(decStart).Seconds()
		}

		if (i+1)%10 == 0 || i+1 == len(files) {
			fmt.Fprintf(os.Stderr, "\r  otel-arrow: %d/%d chunks", i+1, len(files))
		}
	}
	fmt.Fprintf(os.Stderr, "\r  otel-arrow: %d/%d chunks (encode %.1fs, decode %.1fs)\n",
		len(files), len(files), totalEncSecs, totalDecSecs)

	return OtelArrowResult{
		EncodedSize:   totalSize,
		EncodeSeconds: totalEncSecs,
		DecodeSeconds: totalDecSecs,
	}, nil
}

// encodeArrowSubBatches splits td into sub-batches of at most maxSpans spans,
// encodes each with a fresh Producer, and returns the proto-marshalled
// BatchArrowRecords for each sub-batch.
func encodeArrowSubBatches(td ptrace.Traces, maxSpans int) ([][]byte, error) {
	var results [][]byte
	for _, sub := range splitTracesBySpanCount(td, maxSpans) {
		// Uint32LimitDictIndex keeps non-attribute dictionaries (e.g. span
		// names) from falling back to plain encoding unnecessarily.
		producer := arrow_record.NewProducerWithOptions(cfg.WithUint32LimitDictIndex())
		batch, err := producer.BatchArrowRecordsFromTraces(sub)
		if err != nil {
			producer.Close()
			return nil, fmt.Errorf("producer.BatchArrowRecordsFromTraces: %w", err)
		}
		encoded, err := proto.Marshal(batch)
		if err != nil {
			producer.Close()
			return nil, fmt.Errorf("proto.Marshal: %w", err)
		}
		if err := producer.Close(); err != nil {
			return nil, fmt.Errorf("producer.Close: %w", err)
		}
		results = append(results, encoded)
	}
	return results, nil
}

// splitTracesBySpanCount slices td into ptrace.Traces objects of at most
// maxSpans spans each, walking (ResourceSpans, ScopeSpans, Span) in order.
// A single source span is never split across sub-batches; the surrounding
// ResourceSpans and ScopeSpans wrappers are duplicated (resource + scope
// attributes copied) into whatever sub-batch that span lands in.
//
// This is enough for cost measurement — the upstream exporter's batch
// processor does similar slicing. We don't bother to merge consecutive
// scope groups within a sub-batch; the resulting wire size is negligibly
// larger and Arrow's dictionary will collapse repeated values anyway.
func splitTracesBySpanCount(td ptrace.Traces, maxSpans int) []ptrace.Traces {
	if td.SpanCount() <= maxSpans {
		return []ptrace.Traces{td}
	}

	var out []ptrace.Traces
	current := ptrace.NewTraces()
	spanBudget := maxSpans

	// Track the most recently used (ri, si) pair in the current sub-batch so
	// consecutive spans from the same source scope still share a single
	// ScopeSpans entry within the sub-batch.
	var curRS ptrace.ResourceSpans
	var curSS ptrace.ScopeSpans
	var haveCur bool
	lastRI, lastSI := -1, -1

	flush := func() {
		if current.SpanCount() > 0 {
			out = append(out, current)
		}
		current = ptrace.NewTraces()
		spanBudget = maxSpans
		haveCur = false
		lastRI, lastSI = -1, -1
	}

	rsList := td.ResourceSpans()
	for ri := 0; ri < rsList.Len(); ri++ {
		srcRS := rsList.At(ri)
		ssList := srcRS.ScopeSpans()
		for si := 0; si < ssList.Len(); si++ {
			srcSS := ssList.At(si)
			spans := srcSS.Spans()
			for spi := 0; spi < spans.Len(); spi++ {
				if spanBudget == 0 {
					flush()
				}
				if !haveCur || lastRI != ri {
					curRS = current.ResourceSpans().AppendEmpty()
					srcRS.Resource().CopyTo(curRS.Resource())
					curRS.SetSchemaUrl(srcRS.SchemaUrl())
					lastRI = ri
					lastSI = -1
					haveCur = true
				}
				if lastSI != si {
					curSS = curRS.ScopeSpans().AppendEmpty()
					srcSS.Scope().CopyTo(curSS.Scope())
					curSS.SetSchemaUrl(srcSS.SchemaUrl())
					lastSI = si
				}
				spans.At(spi).CopyTo(curSS.Spans().AppendEmpty())
				spanBudget--
			}
		}
	}
	flush()
	return out
}
