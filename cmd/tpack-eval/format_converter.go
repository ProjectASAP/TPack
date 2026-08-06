package main

import (
	"bufio"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"maps"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// bucketSub identifies a chunk sub-file by (bucket, sub-index).
type bucketSub struct {
	bucket int64
	subIdx int
}

// compactSpan holds only the fields needed for duration adjustment.
// 24 bytes per entry — ~1.4 GB for 17M spans (vs ~50 GB in pdata).
type compactSpan struct {
	parentID pcommon.SpanID
	start    pcommon.Timestamp
	end      pcommon.Timestamp
}

// logMemStats prints a one-line snapshot of current Go heap usage + GC counter
// prefixed by label. Used for OOM diagnosis during --transform at large N.
// Forces a GC first so the numbers reflect live (reachable) memory, not
// retained garbage.
func logMemStats(label string) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	const mib = 1 << 20
	log.Printf("MemStats [%s]: HeapAlloc=%d MiB  HeapInuse=%d MiB  Sys=%d MiB  NumGC=%d",
		label,
		ms.HeapAlloc/mib,
		ms.HeapInuse/mib,
		ms.Sys/mib,
		ms.NumGC,
	)
}

// timestampAdj holds adjusted timestamps for a span.
type timestampAdj struct {
	start pcommon.Timestamp
	end   pcommon.Timestamp
}

// chunkWriter accumulates OTLP traces and writes them as a single protobuf binary file.
type chunkWriter struct {
	path      string
	traces    ptrace.Traces // accumulated traces for this chunk
	spanCount int           // number of spans accumulated
	subIdx    int           // sub-index for chunk splitting (0, 1, 2, ...)
	bucket    int64         // time bucket key
}

// fileReader reads an input file and returns OTLP Traces.
type fileReader func(string) (ptrace.Traces, error)

// detectInputFormat auto-detects the input format and returns a reader + file list.
// Supported formats: CSV (RE2), Jaeger JSON directory, OTLP JSONL directory, OTLP JSON chunks.
func detectInputFormat(inputPath string) (reader fileReader, files []string, format string, err error) {
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("stat %s: %w", inputPath, err)
	}

	if !info.IsDir() && strings.HasSuffix(strings.ToLower(inputPath), ".csv") {
		return readCSVFile, []string{inputPath}, "csv", nil
	}
	if !info.IsDir() {
		return nil, nil, "", fmt.Errorf("%s is not a directory (and not a .csv file)", inputPath)
	}

	files, err = filepath.Glob(filepath.Join(inputPath, "*.json"))
	if err != nil {
		return nil, nil, "", fmt.Errorf("glob %s: %w", inputPath, err)
	}
	pbFiles, _ := filepath.Glob(filepath.Join(inputPath, "*.pb"))
	files = append(files, pbFiles...)
	if len(files) == 0 {
		// Check for RE2-style subdirectories
		entries, err2 := os.ReadDir(inputPath)
		if err2 == nil {
			for _, e := range entries {
				if e.IsDir() {
					return readCSVFile, nil, "re2", nil // handled separately by statsRE2Dir
				}
			}
		}
		return nil, nil, "", fmt.Errorf("no .json or .pb files in %s", inputPath)
	}
	sort.Strings(files)

	detected, fmtErr := detectJSONFormat(files[0])
	if fmtErr == nil && detected == "jaeger" {
		return readJaegerFile, files, "jaeger", nil
	}
	// readOTLPFile handles both plain JSON and JSONL
	return readOTLPFile, files, "otlp", nil
}

// runTransform auto-detects input type (CSV, Jaeger JSON dir, OTLP JSONL dir)
// and converts to chunked OTLP JSON with duration adjustment.
//
// Two-pass approach to avoid OOM on large datasets:
//
//	Pass 1: collect compact timing data for duration adjustment + trace bucketing
//	Pass 2: re-read files, prune, apply adjustments, route to chunk writers
//
// If remap is true: shift all traces into [0, 60s), discard traces with root duration > 60s.
func runTransform(inputPath, outputPath string, primaryAttributes, dependentAttributes []string, maxTraces int, maxSpansPerChunk int, remap bool, bucketDurationUs int64) error {
	reader, files, format, err := detectInputFormat(inputPath)
	if err != nil {
		return err
	}

	log.Printf("Transform: reading %d %s files from %s", len(files), format, inputPath)

	// Jaeger: 1 file = 1 trace. When --remap is used, some traces are discarded
	// (root duration > 60s), so we over-read and subsample after filtering.
	if format == "jaeger" && maxTraces > 0 && maxTraces < len(files) {
		subRng := rand.New(rand.NewSource(42))
		subRng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
		if remap {
			// Over-read by 15% to compensate for remap filtering
			overRead := min(int(float64(maxTraces)*1.15), len(files))
			files = files[:overRead]
		} else {
			files = files[:maxTraces]
		}
		sort.Strings(files)
		log.Printf("Transform: subsampled to %d files", len(files))
	}

	// Build set of attributes to keep: non-well-known feature columns + metadata columns
	keepAttrs := make(map[string]bool)
	for _, col := range primaryAttributes {
		if !tpackmodel.WellKnownColumns[col] {
			keepAttrs[col] = true
		}
	}
	for _, col := range dependentAttributes {
		keepAttrs[col] = true
	}

	logMemStats("transform: start")

	// --- Pass 1: Index spans for duration adjustment + trace min start ---
	//
	// For Jaeger format (1 file = 1 trace), we compute duration adjustments
	// per-file to avoid holding a global spanIndex in memory. This reduces
	// peak memory from O(total_spans) to O(max_trace_size) — critical for
	// large datasets like Uber (500K+ traces, 700M+ spans).
	traceMinStart := make(map[pcommon.TraceID]pcommon.Timestamp)
	traceMaxEnd := make(map[pcommon.TraceID]pcommon.Timestamp) // for remap filtering
	totalSpans := 0
	traceCount := 0
	adjustments := make(map[pcommon.SpanID]timestampAdj)

	// Per-trace span counts (populated in pass 1, consumed after traceBucket is built
	// to determine how many sub-files each bucket needs). See bucketNumSubs below.
	traceSpanCount := make(map[pcommon.TraceID]int)

	// Trace IDs observed in each input file. Pass 2 groups files by
	// (bucket, subIdx) via these so each output chunk is produced one at a
	// time — avoids holding hundreds of chunk writers live at pass-2 end.
	fileTraceIDs := make([][]pcommon.TraceID, len(files))

	if format == "jaeger" {
		// Per-file duration adjustment: each Jaeger file is one trace,
		// so parent-child relationships are file-local. Process files in
		// parallel since each is independent.
		type jaegerResult struct {
			fileIdx        int
			traceIDs       []pcommon.TraceID                        // unique IDs seen in this file
			traceTimings   map[pcommon.TraceID][2]pcommon.Timestamp // [minStart, maxEnd]
			traceSpanCount map[pcommon.TraceID]int
			adjustments    map[pcommon.SpanID]timestampAdj
			spanCount      int
		}

		numWorkers := runtime.NumCPU()
		results := make(chan jaegerResult, numWorkers*2)
		fileCh := make(chan int, numWorkers*2)

		var wg sync.WaitGroup
		for range numWorkers {
			wg.Go(func() {
				for fi := range fileCh {
					td, err := reader(files[fi])
					if err != nil {
						log.Printf("Transform: warning: read %s: %v", files[fi], err)
						continue
					}

					localIndex := make(map[pcommon.SpanID]compactSpan)
					timings := make(map[pcommon.TraceID][2]pcommon.Timestamp)
					localCount := make(map[pcommon.TraceID]int)
					for i := 0; i < td.ResourceSpans().Len(); i++ {
						rs := td.ResourceSpans().At(i)
						for j := 0; j < rs.ScopeSpans().Len(); j++ {
							ss := rs.ScopeSpans().At(j)
							for k := 0; k < ss.Spans().Len(); k++ {
								span := ss.Spans().At(k)
								tid := span.TraceID()

								localIndex[span.SpanID()] = compactSpan{
									parentID: span.ParentSpanID(),
									start:    span.StartTimestamp(),
									end:      span.EndTimestamp(),
								}
								localCount[tid]++

								if prev, ok := timings[tid]; !ok {
									timings[tid] = [2]pcommon.Timestamp{span.StartTimestamp(), span.EndTimestamp()}
								} else {
									if span.StartTimestamp() < prev[0] {
										prev[0] = span.StartTimestamp()
									}
									if span.EndTimestamp() > prev[1] {
										prev[1] = span.EndTimestamp()
									}
									timings[tid] = prev
								}
							}
						}
					}

					localAdj := computeDurationAdjustments(localIndex)
					spanCount := len(localIndex)

					tids := make([]pcommon.TraceID, 0, len(timings))
					for tid := range timings {
						tids = append(tids, tid)
					}

					results <- jaegerResult{
						fileIdx:        fi,
						traceIDs:       tids,
						traceTimings:   timings,
						traceSpanCount: localCount,
						adjustments:    localAdj,
						spanCount:      spanCount,
					}
				}
			})
		}

		// Feed files to workers
		go func() {
			for fi := range files {
				fileCh <- fi
			}
			close(fileCh)
		}()
		// Close results channel once all workers finish
		go func() {
			wg.Wait()
			close(results)
		}()

		// Merge results in main goroutine
		processed := 0
		for r := range results {
			fileTraceIDs[r.fileIdx] = r.traceIDs
			for tid, ts := range r.traceTimings {
				if prev, ok := traceMinStart[tid]; !ok {
					traceMinStart[tid] = ts[0]
					traceMaxEnd[tid] = ts[1]
					traceCount++
				} else {
					if ts[0] < prev {
						traceMinStart[tid] = ts[0]
					}
					if ts[1] > traceMaxEnd[tid] {
						traceMaxEnd[tid] = ts[1]
					}
				}
			}
			maps.Copy(adjustments, r.adjustments)
			for tid, c := range r.traceSpanCount {
				traceSpanCount[tid] += c
			}
			totalSpans += r.spanCount
			processed++
			if processed%1000 == 0 || processed == len(files) {
				log.Printf("Transform: pass 1 — processed %d/%d files (%d spans, %d traces, %d adjustments)", processed, len(files), totalSpans, traceCount, len(adjustments))
			}
		}
	} else {
		// Global spanIndex approach for non-Jaeger formats (smaller datasets)
		spanIndex := make(map[pcommon.SpanID]compactSpan)
		for fi, file := range files {
			td, err := reader(file)
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}

			fileTidSet := make(map[pcommon.TraceID]bool)
			for i := 0; i < td.ResourceSpans().Len(); i++ {
				rs := td.ResourceSpans().At(i)
				for j := 0; j < rs.ScopeSpans().Len(); j++ {
					ss := rs.ScopeSpans().At(j)
					for k := 0; k < ss.Spans().Len(); k++ {
						span := ss.Spans().At(k)

						tid := span.TraceID()
						// --max-traces for OTLP/CSV: skip new traces once limit reached
						if maxTraces > 0 {
							if _, known := traceMinStart[tid]; !known && traceCount >= maxTraces {
								continue
							}
						}

						spanIndex[span.SpanID()] = compactSpan{
							parentID: span.ParentSpanID(),
							start:    span.StartTimestamp(),
							end:      span.EndTimestamp(),
						}

						if prev, ok := traceMinStart[tid]; !ok {
							traceMinStart[tid] = span.StartTimestamp()
							traceMaxEnd[tid] = span.EndTimestamp()
							traceCount++
						} else {
							if span.StartTimestamp() < prev {
								traceMinStart[tid] = span.StartTimestamp()
							}
							if span.EndTimestamp() > traceMaxEnd[tid] {
								traceMaxEnd[tid] = span.EndTimestamp()
							}
						}

						traceSpanCount[tid]++
						totalSpans++
						fileTidSet[tid] = true
					}
				}
			}
			if len(fileTidSet) > 0 {
				tids := make([]pcommon.TraceID, 0, len(fileTidSet))
				for tid := range fileTidSet {
					tids = append(tids, tid)
				}
				fileTraceIDs[fi] = tids
			}

			if (fi+1)%100 == 0 || fi+1 == len(files) {
				log.Printf("Transform: pass 1 — indexed %d/%d files (%d spans, %d traces)", fi+1, len(files), totalSpans, traceCount)
			}
			if (fi+1)%10000 == 0 {
				runtime.GC()
			}
		}

		adjustments = computeDurationAdjustments(spanIndex)
		spanIndex = nil
		runtime.GC()
	}
	log.Printf("Transform: computed %d duration adjustments", len(adjustments))

	logMemStats(fmt.Sprintf("after pass1: adjustments=%d traceMinStart=%d", len(adjustments), len(traceMinStart)))

	// --- Pass 1c: Remap timestamps (if --remap) ---
	const remapWindowUs = int64(60_000_000) // 60 seconds in µs
	const remapWindowNs = remapWindowUs * 1000
	traceTimeShift := map[pcommon.TraceID]int64{} // ns shift per trace

	if remap {
		rng := rand.New(rand.NewSource(42))
		discarded := 0
		for tid, minStart := range traceMinStart {
			// Compute adjusted root duration for this trace
			maxEnd := traceMaxEnd[tid]
			// Apply duration adjustments to get true max end
			// (adjustments expand parents, so traceMaxEnd is a lower bound — good enough)
			traceDurUs := (int64(maxEnd) - int64(minStart)) / 1000
			if traceDurUs > remapWindowUs {
				discarded++
				delete(traceMinStart, tid)
				continue
			}
			// Shift root to random offset in [0, 60s), ensuring trace fits
			maxOffset := max(remapWindowUs-traceDurUs, 0)
			newBaseUs := rng.Int63n(maxOffset + 1)
			shiftUs := newBaseUs - int64(minStart)/1000
			traceTimeShift[tid] = shiftUs * 1000 // store as ns
		}
		log.Printf("Transform: --remap: kept %d traces, discarded %d (root duration > 60s)", len(traceMinStart), discarded)

		// Subsample to exactly maxTraces after remap filtering
		if maxTraces > 0 && len(traceMinStart) > maxTraces {
			tids := make([]pcommon.TraceID, 0, len(traceMinStart))
			for tid := range traceMinStart {
				tids = append(tids, tid)
			}
			// Sort before shuffling: Go randomizes map iteration order, so
			// shuffling straight off the range would pick a different subset on
			// every run even though rng is seeded. Sorting first makes the
			// transform reproducible, which the evaluation depends on.
			sort.Slice(tids, func(i, j int) bool {
				return string(tids[i][:]) < string(tids[j][:])
			})
			rng.Shuffle(len(tids), func(i, j int) { tids[i], tids[j] = tids[j], tids[i] })
			for _, tid := range tids[maxTraces:] {
				delete(traceMinStart, tid)
				delete(traceTimeShift, tid)
			}
			log.Printf("Transform: --remap: subsampled to %d traces", maxTraces)
		}
	}
	traceMaxEnd = nil

	// Compute trace → chunk bucket assignments using evaluation bucket duration
	bucketDurationNs := bucketDurationUs * 1000
	traceBucket := make(map[pcommon.TraceID]int64, len(traceMinStart))
	if remap {
		for tid := range traceMinStart {
			traceBucket[tid] = 0
		}
	} else {
		for tid, minStart := range traceMinStart {
			traceBucket[tid] = int64(minStart) / bucketDurationNs
		}
	}
	traceMinStart = nil // free
	log.Printf("Transform: %d traces across %d buckets", len(traceBucket), countDistinctValues(traceBucket))

	// Compute per-bucket sub-file count based on post-discard span totals, then
	// assign each trace to a deterministic sub-file via FNV hash so all of a
	// trace's spans land in one file (correctness for template extraction).
	bucketTotals := make(map[int64]int64, 0)
	for tid, b := range traceBucket {
		bucketTotals[b] += int64(traceSpanCount[tid])
	}
	bucketNumSubs := make(map[int64]int, len(bucketTotals))
	for b, total := range bucketTotals {
		n := 1
		if maxSpansPerChunk > 0 {
			n = max(int((total+int64(maxSpansPerChunk)-1)/int64(maxSpansPerChunk)), 1)
		}
		bucketNumSubs[b] = n
	}
	traceSubIdx := make(map[pcommon.TraceID]int, len(traceBucket))
	for tid, b := range traceBucket {
		k := bucketNumSubs[b]
		if k <= 1 {
			continue // zero value == sub 0
		}
		h := fnv.New64a()
		h.Write(tid[:])
		traceSubIdx[tid] = int(h.Sum64() % uint64(k))
	}
	traceSpanCount = nil // free

	// Group input files by their target output chunk (bucket, subIdx). A Jaeger
	// file typically contains one trace → one key; OTLP/CSV files can span
	// multiple traces → multiple keys, in which case the file is processed
	// once per distinct key it contributes spans to. Processing one chunk at
	// a time keeps peak memory bounded by a single chunk's span set instead
	// of all chunks simultaneously.
	groups := make(map[bucketSub][]int)
	for fi, tids := range fileTraceIDs {
		fileKeys := make(map[bucketSub]bool)
		for _, tid := range tids {
			b, ok := traceBucket[tid]
			if !ok {
				continue
			}
			fileKeys[bucketSub{b, traceSubIdx[tid]}] = true
		}
		for k := range fileKeys {
			groups[k] = append(groups[k], fi)
		}
	}

	keys := make([]bucketSub, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].bucket != keys[j].bucket {
			return keys[i].bucket < keys[j].bucket
		}
		return keys[i].subIdx < keys[j].subIdx
	})

	logMemStats(fmt.Sprintf("before pass2: chunks=%d", len(keys)))

	// --- Pass 2: Re-read, prune, adjust, route to chunk writers ---
	if err := os.MkdirAll(outputPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outputPath, err)
	}

	totalSpansWritten := 0
	for ki, key := range keys {
		cw := &chunkWriter{
			path:   filepath.Join(outputPath, fmt.Sprintf("chunk_%020d_%04d.pb", key.bucket, key.subIdx)),
			traces: ptrace.NewTraces(),
			bucket: key.bucket,
			subIdx: key.subIdx,
		}

		for _, fi := range groups[key] {
			td, err := reader(files[fi])
			if err != nil {
				return fmt.Errorf("read %s: %w", files[fi], err)
			}

			pruneAttributes(td, keepAttrs)
			applyDurationAdjustments(td, adjustments)
			if remap {
				applyTimeShifts(td, traceTimeShift)
			}

			// Copy spans whose trace maps to this specific key. For a
			// 1-file-1-trace Jaeger input this copies all spans in the file;
			// for multi-trace files only the matching subset lands here.
			type spanLoc struct {
				scopeIdx int
				spanIdx  int
			}
			for i := 0; i < td.ResourceSpans().Len(); i++ {
				rs := td.ResourceSpans().At(i)
				var locs []spanLoc
				for j := 0; j < rs.ScopeSpans().Len(); j++ {
					ss := rs.ScopeSpans().At(j)
					for k := 0; k < ss.Spans().Len(); k++ {
						span := ss.Spans().At(k)
						tid := span.TraceID()
						b, ok := traceBucket[tid]
						if !ok {
							continue
						}
						if b != key.bucket || traceSubIdx[tid] != key.subIdx {
							continue
						}
						locs = append(locs, spanLoc{j, k})
					}
				}
				if len(locs) == 0 {
					continue
				}
				miniRS := cw.traces.ResourceSpans().AppendEmpty()
				rs.Resource().CopyTo(miniRS.Resource())
				miniSS := miniRS.ScopeSpans().AppendEmpty()
				for _, loc := range locs {
					srcSpan := rs.ScopeSpans().At(loc.scopeIdx).Spans().At(loc.spanIdx)
					srcSpan.CopyTo(miniSS.Spans().AppendEmpty())
				}
				cw.spanCount += len(locs)
			}
		}

		if err := closeOneChunkWriter(cw); err != nil {
			return err
		}
		totalSpansWritten += cw.spanCount
		// Drop the now-marshaled pdata so GC can reclaim it before we build
		// the next chunk.
		cw.traces = ptrace.NewTraces()

		if (ki+1)%100 == 0 || ki+1 == len(keys) {
			log.Printf("Transform: pass 2 — wrote %d/%d chunks (%d spans total)", ki+1, len(keys), totalSpansWritten)
		}
		if (ki+1)%100 == 0 {
			runtime.GC()
			logMemStats(fmt.Sprintf("pass2 ki=%d spans=%d", ki+1, totalSpansWritten))
		}
	}

	logMemStats(fmt.Sprintf("after pass2: chunks=%d spans=%d", len(keys), totalSpansWritten))

	log.Printf("Transform: wrote %d traces (%d spans) across %d chunk files to %s", len(traceBucket), totalSpansWritten, len(keys), outputPath)

	// Benchmark raw gzip and cache results alongside chunk files
	log.Printf("Transform: benchmarking raw gzip...")
	gzResult, err := benchmarkGzip(outputPath, 0, true, "")
	if err != nil {
		log.Printf("Warning: raw gzip benchmark failed: %v", err)
	} else {
		// Compute total raw file size
		var rawBytes int64
		jsonFiles, _ := filepath.Glob(filepath.Join(outputPath, "*.json"))
		pbFiles, _ := filepath.Glob(filepath.Join(outputPath, "*.pb"))
		for _, f := range append(jsonFiles, pbFiles...) {
			if info, err := os.Stat(f); err == nil {
				rawBytes += info.Size()
			}
		}
		for name, val := range map[string]string{
			"raw_bytes":                   fmt.Sprintf("%d", rawBytes),
			"raw_gz_bytes":                fmt.Sprintf("%d", gzResult.CompressedSize),
			"raw_gzip_compress_seconds":   fmt.Sprintf("%.6f", gzResult.CompressSeconds),
			"raw_gzip_decompress_seconds": fmt.Sprintf("%.6f", gzResult.DecompressSeconds),
		} {
			os.WriteFile(filepath.Join(outputPath, name), []byte(val), 0o644)
		}
		log.Printf("Transform: raw size %d bytes, gzip %d bytes (%.1fx), compress %.1fs, decompress %.1fs",
			rawBytes, gzResult.CompressedSize, float64(rawBytes)/float64(gzResult.CompressedSize),
			gzResult.CompressSeconds, gzResult.DecompressSeconds)
	}

	return nil
}

// closeOneChunkWriter marshals accumulated traces to protobuf and writes the file.
func closeOneChunkWriter(cw *chunkWriter) error {
	marshaler := &ptrace.ProtoMarshaler{}
	data, err := marshaler.MarshalTraces(cw.traces)
	if err != nil {
		return fmt.Errorf("marshal chunk %d_%d: %w", cw.bucket, cw.subIdx, err)
	}
	if err := os.WriteFile(cw.path, data, 0o644); err != nil {
		return fmt.Errorf("write chunk %s: %w", cw.path, err)
	}
	return nil
}

// countDistinctValues counts the number of distinct values in a map.
func countDistinctValues(m map[pcommon.TraceID]int64) int {
	seen := make(map[int64]struct{})
	for _, v := range m {
		seen[v] = struct{}{}
	}
	return len(seen)
}

// computeDurationAdjustments runs leaf-to-root BFS on compact span data
// and returns adjusted timestamps for spans whose parents need expanding.
func computeDurationAdjustments(spans map[pcommon.SpanID]compactSpan) map[pcommon.SpanID]timestampAdj {
	// Build inDegree (count of children per span)
	inDegree := make(map[pcommon.SpanID]int, len(spans)/4)
	for _, cs := range spans {
		if !cs.parentID.IsEmpty() {
			inDegree[cs.parentID]++
		}
	}

	// Start with leaves (no children)
	queue := make([]pcommon.SpanID, 0, len(spans)/2)
	for sid := range spans {
		if inDegree[sid] == 0 {
			queue = append(queue, sid)
		}
	}

	// Track adjusted timestamps — only for spans that actually changed
	adjusted := make(map[pcommon.SpanID]timestampAdj)

	getTimestamps := func(sid pcommon.SpanID) (pcommon.Timestamp, pcommon.Timestamp) {
		if adj, ok := adjusted[sid]; ok {
			return adj.start, adj.end
		}
		cs := spans[sid]
		return cs.start, cs.end
	}

	for len(queue) > 0 {
		sid := queue[0]
		queue = queue[1:]

		cs, ok := spans[sid]
		if !ok || cs.parentID.IsEmpty() {
			continue
		}

		childStart, childEnd := getTimestamps(sid)
		parentStart, parentEnd := getTimestamps(cs.parentID)

		changed := false
		newStart, newEnd := parentStart, parentEnd

		if childEnd > parentEnd {
			newEnd = childEnd
			changed = true
		}
		if childStart < parentStart {
			newStart = childStart
			changed = true
		}

		if changed {
			adjusted[cs.parentID] = timestampAdj{start: newStart, end: newEnd}
		}

		inDegree[cs.parentID]--
		if inDegree[cs.parentID] == 0 {
			queue = append(queue, cs.parentID)
		}
	}

	return adjusted
}

// pruneAttributes removes all span attributes except those in keepAttrs.
func pruneAttributes(td ptrace.Traces, keepAttrs map[string]bool) {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				span.Attributes().RemoveIf(func(key string, _ pcommon.Value) bool {
					return !keepAttrs[key]
				})
			}
		}
	}
}

// applyDurationAdjustments applies pre-computed timestamp changes to spans.
func applyDurationAdjustments(td ptrace.Traces, adjustments map[pcommon.SpanID]timestampAdj) {
	if len(adjustments) == 0 {
		return
	}
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				if adj, ok := adjustments[span.SpanID()]; ok {
					span.SetStartTimestamp(adj.start)
					span.SetEndTimestamp(adj.end)
				}
			}
		}
	}
}

// applyTimeShifts shifts all span timestamps by the per-trace shift amount.
func applyTimeShifts(td ptrace.Traces, shifts map[pcommon.TraceID]int64) {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				shift, ok := shifts[span.TraceID()]
				if !ok {
					continue
				}
				span.SetStartTimestamp(pcommon.Timestamp(int64(span.StartTimestamp()) + shift))
				span.SetEndTimestamp(pcommon.Timestamp(int64(span.EndTimestamp()) + shift))
			}
		}
	}
}

// readJSONLFile reads a JSONL file where each line is a complete OTLP JSON object.
func readJSONLFile(path string) (ptrace.Traces, error) {
	f, err := os.Open(path)
	if err != nil {
		return ptrace.Traces{}, err
	}
	defer f.Close()

	merged := ptrace.NewTraces()
	unmarshaler := &ptrace.JSONUnmarshaler{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		td, err := unmarshaler.UnmarshalTraces(line)
		if err != nil {
			continue // skip malformed lines
		}

		td.ResourceSpans().MoveAndAppendTo(merged.ResourceSpans())
	}

	if err := scanner.Err(); err != nil {
		return ptrace.Traces{}, fmt.Errorf("scan %s: %w", path, err)
	}

	return merged, nil
}

// readJaegerFile reads a single Jaeger JSON file and converts to ptrace.Traces.
// Keeps original timestamps (no shifting — the unified pipeline handles remapping).
func readJaegerFile(path string) (ptrace.Traces, error) {
	fh, err := os.Open(path)
	if err != nil {
		return ptrace.Traces{}, err
	}
	defer fh.Close()

	var data struct {
		Data []struct {
			TraceID string `json:"traceID"`
			Spans   []struct {
				SpanID        string `json:"spanID"`
				OperationName string `json:"operationName"`
				StartTime     int64  `json:"startTime"`
				Duration      int64  `json:"duration"`
				ProcessID     string `json:"processID"`
				References    []struct {
					RefType string `json:"refType"`
					SpanID  string `json:"spanID"`
				} `json:"references"`
				Tags []struct {
					Key   string `json:"key"`
					Value any    `json:"value"`
				} `json:"tags"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
		} `json:"data"`
	}

	if err := json.NewDecoder(fh).Decode(&data); err != nil {
		return ptrace.Traces{}, fmt.Errorf("parse %s: %w", path, err)
	}

	td := ptrace.NewTraces()

	for _, trace := range data.Data {
		if len(trace.Spans) == 0 {
			continue
		}

		// Group spans by service
		type jaegerSpan struct {
			spanID   string
			parentID string
			opName   string
			start    int64
			duration int64
			tags     map[string]string
		}
		svcSpans := make(map[string][]jaegerSpan)

		for _, s := range trace.Spans {
			svcName := ""
			if proc, ok := trace.Processes[s.ProcessID]; ok {
				svcName = proc.ServiceName
			}

			parentID := ""
			for _, ref := range s.References {
				if ref.RefType == "CHILD_OF" {
					parentID = ref.SpanID
					break
				}
			}

			tags := make(map[string]string)
			for _, tag := range s.Tags {
				switch v := tag.Value.(type) {
				case bool:
					if v {
						tags[tag.Key] = "true"
					} else {
						tags[tag.Key] = "false"
					}
				case string:
					tags[tag.Key] = v
				case float64:
					tags[tag.Key] = fmt.Sprintf("%v", v)
				}
			}

			svcSpans[svcName] = append(svcSpans[svcName], jaegerSpan{
				spanID:   s.SpanID,
				parentID: parentID,
				opName:   s.OperationName,
				start:    s.StartTime, // original µs timestamp
				duration: s.Duration,
				tags:     tags,
			})
		}

		// Build OTLP ResourceSpans grouped by service
		for svcName, spans := range svcSpans {
			rs := td.ResourceSpans().AppendEmpty()
			rs.Resource().Attributes().PutStr("service.name", svcName)
			ss := rs.ScopeSpans().AppendEmpty()

			for _, s := range spans {
				span := ss.Spans().AppendEmpty()

				paddedTraceHex := strings.Repeat("0", 32-len(trace.TraceID)) + trace.TraceID
				traceIDBytes, _ := hex.DecodeString(paddedTraceHex)
				var tid pcommon.TraceID
				copy(tid[:], traceIDBytes)
				span.SetTraceID(tid)

				spanIDBytes, _ := hex.DecodeString(s.spanID)
				var sid pcommon.SpanID
				copy(sid[8-len(spanIDBytes):], spanIDBytes)
				span.SetSpanID(sid)

				if s.parentID != "" {
					parentIDBytes, _ := hex.DecodeString(s.parentID)
					var pid pcommon.SpanID
					copy(pid[8-len(parentIDBytes):], parentIDBytes)
					span.SetParentSpanID(pid)
				}

				span.SetName(s.opName)
				span.SetStartTimestamp(pcommon.Timestamp(s.start * 1000))              // µs → ns
				span.SetEndTimestamp(pcommon.Timestamp((s.start + s.duration) * 1000)) // µs → ns

				// Map Jaeger error tag to OTLP status code
				if s.tags["error"] == "true" {
					span.Status().SetCode(ptrace.StatusCodeError)
				}

				for k, v := range s.tags {
					if k == "error" {
						continue // already mapped to status.code
					}
					span.Attributes().PutStr(k, v)
				}
			}
		}
	}

	return td, nil
}

// countPdataSpans counts total spans in ptrace.Traces.
func countPdataSpans(td ptrace.Traces) int {
	count := 0
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			count += rs.ScopeSpans().At(j).Spans().Len()
		}
	}
	return count
}

// readCSVFile reads a RE2-format CSV and returns ptrace.Traces.
//
// Required columns: traceID, spanID, serviceName, methodName, operationName,
// startTime (μs), duration (μs), parentSpanID.
// Optional columns: statusCode (or status.code), spanKind; extra columns
// become span attributes.
func readCSVFile(csvPath string) (ptrace.Traces, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return ptrace.Traces{}, fmt.Errorf("open %s: %w", csvPath, err)
	}
	defer f.Close()

	csvReader := csv.NewReader(f)
	header, err := csvReader.Read()
	if err != nil {
		return ptrace.Traces{}, fmt.Errorf("read CSV header: %w", err)
	}

	// Build column index
	colIdx := make(map[string]int, len(header))
	for i, col := range header {
		colIdx[strings.TrimSpace(col)] = i
	}

	// Validate required columns
	required := []string{"traceID", "spanID", "serviceName", "methodName", "operationName", "startTime", "duration", "parentSpanID"}
	for _, col := range required {
		if _, ok := colIdx[col]; !ok {
			return ptrace.Traces{}, fmt.Errorf("CSV missing required column %q", col)
		}
	}

	// Identify structural columns (not stored as extra attributes)
	structural := map[string]bool{
		"time": true, "traceID": true, "spanID": true,
		"serviceName": true, "methodName": true, "operationName": true,
		"startTimeMillis": true, "startTime": true, "duration": true,
		"statusCode": true, "status.code": true, "spanKind": true,
		"parentSpanID": true,
	}

	// Identify extra columns → span attributes
	var extraCols []string
	for _, col := range header {
		col = strings.TrimSpace(col)
		if !structural[col] {
			extraCols = append(extraCols, col)
		}
	}

	// Determine statusCode column name
	statusCol := ""
	if _, ok := colIdx["statusCode"]; ok {
		statusCol = "statusCode"
	} else if _, ok := colIdx["status.code"]; ok {
		statusCol = "status.code"
	}

	spanKindCol := ""
	if _, ok := colIdx["spanKind"]; ok {
		spanKindCol = "spanKind"
	}

	// Parse rows and group by service
	type csvSpan struct {
		traceID      string
		spanID       string
		parentSpanID string
		opName       string
		methodName   string
		startTimeUs  int64
		durationUs   int64
		statusCode   int // 0=unset, 1=ok, 2=error
		spanKind     int // OTel integer
		extraAttrs   map[string]string
	}

	serviceSpans := make(map[string][]csvSpan)
	rowNum := 1 // 1-indexed (header is row 1)

	for {
		row, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ptrace.Traces{}, fmt.Errorf("read CSV row %d: %w", rowNum+1, err)
		}
		rowNum++

		getCol := func(name string) string {
			if idx, ok := colIdx[name]; ok && idx < len(row) {
				return strings.TrimSpace(row[idx])
			}
			return ""
		}

		traceID := getCol("traceID")
		spanID := getCol("spanID")
		if traceID == "" || spanID == "" {
			continue // skip rows without IDs
		}

		svc := getCol("serviceName")
		if svc == "" {
			svc = "unknown"
		}

		startTimeUs, err := strconv.ParseInt(getCol("startTime"), 10, 64)
		if err != nil {
			log.Printf("CSV row %d: invalid startTime %q, skipping", rowNum, getCol("startTime"))
			continue
		}

		durationUs, err := strconv.ParseInt(getCol("duration"), 10, 64)
		if err != nil {
			log.Printf("CSV row %d: invalid duration %q, skipping", rowNum, getCol("duration"))
			continue
		}

		// Parse statusCode: "0.0"→0, "1.0"→1, "2.0"→2, ""→0
		sc := 0
		if statusCol != "" {
			scStr := getCol(statusCol)
			if scStr != "" {
				scFloat, err := strconv.ParseFloat(scStr, 64)
				if err == nil {
					sc = int(math.Round(scFloat))
				}
			}
		}

		// Parse spanKind if present
		sk := 0
		if spanKindCol != "" {
			skStr := getCol(spanKindCol)
			if skStr != "" {
				sk = tpackmodel.SpanKindToOTLP(skStr)
			}
		}

		// Collect extra attributes
		var extras map[string]string
		if len(extraCols) > 0 {
			extras = make(map[string]string, len(extraCols))
			for _, col := range extraCols {
				v := getCol(col)
				if v != "" {
					extras[col] = v
				}
			}
		}

		serviceSpans[svc] = append(serviceSpans[svc], csvSpan{
			traceID:      padHex(traceID, 32),
			spanID:       padHex(spanID, 16),
			parentSpanID: padHex(getCol("parentSpanID"), 16),
			opName:       getCol("operationName"),
			methodName:   getCol("methodName"),
			startTimeUs:  startTimeUs,
			durationUs:   durationUs,
			statusCode:   sc,
			spanKind:     sk,
			extraAttrs:   extras,
		})
	}

	totalSpans := 0
	for _, spans := range serviceSpans {
		totalSpans += len(spans)
	}
	// Build pdata
	td := ptrace.NewTraces()
	for svc, spans := range serviceSpans {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", svc)
		ss := rs.ScopeSpans().AppendEmpty()

		for _, cs := range spans {
			s := ss.Spans().AppendEmpty()

			var tid pcommon.TraceID
			otlpconv.HexToBytes(cs.traceID, tid[:])
			s.SetTraceID(tid)

			var sid pcommon.SpanID
			otlpconv.HexToBytes(cs.spanID, sid[:])
			s.SetSpanID(sid)

			if cs.parentSpanID != "" {
				var psid pcommon.SpanID
				otlpconv.HexToBytes(cs.parentSpanID, psid[:])
				if !psid.IsEmpty() {
					s.SetParentSpanID(psid)
				}
			}

			s.SetName(cs.opName)
			s.SetKind(ptrace.SpanKind(cs.spanKind))

			// μs → ns
			s.SetStartTimestamp(pcommon.Timestamp(cs.startTimeUs * 1000))
			s.SetEndTimestamp(pcommon.Timestamp((cs.startTimeUs + cs.durationUs) * 1000))

			if cs.statusCode > 1 {
				// OTLP 2=Error, gRPC 4/13/14=error — all map to Error
				s.Status().SetCode(ptrace.StatusCodeError)
			} else if cs.statusCode == 1 {
				s.Status().SetCode(ptrace.StatusCodeOk)
			} else {
				s.Status().SetCode(ptrace.StatusCodeUnset)
			}

			// Store methodName as a regular span attribute
			if cs.methodName != "" {
				s.Attributes().PutStr("method.name", cs.methodName)
			}

			// Extra columns → span attributes
			for k, v := range cs.extraAttrs {
				s.Attributes().PutStr(k, v)
			}
		}
	}

	log.Printf("Transform CSV: read %d spans across %d services", totalSpans, len(serviceSpans))
	return td, nil
}

// padHex left-pads a hex string with zeros to the target length.
// Returns "" if the input is empty.
func padHex(h string, targetLen int) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if len(h) >= targetLen {
		return h
	}
	return strings.Repeat("0", targetLen-len(h)) + h
}
