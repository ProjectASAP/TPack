package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// readOTLP reads OTLP traces and groups spans into traces by time bucket.
// Accepts a single file (.json or .pb) or a directory of chunk files.
// For directories, reads one chunk at a time to avoid OOM on large datasets.
func readOTLP(path string, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) (map[int64][]*tpackmodel.Trace, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	if info.IsDir() {
		return readOTLPChunked(path, bucketDurationUs, primaryAttributes, dependentAttributes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var td ptrace.Traces
	if strings.HasSuffix(path, ".json") {
		unmarshaler := &ptrace.JSONUnmarshaler{}
		td, err = unmarshaler.UnmarshalTraces(data)
	} else {
		unmarshaler := &ptrace.ProtoUnmarshaler{}
		td, err = unmarshaler.UnmarshalTraces(data)
	}
	if err != nil {
		return nil, fmt.Errorf("unmarshal OTLP: %w", err)
	}

	traces := otlpconv.FromPdata(td, primaryAttributes, dependentAttributes)

	// Bucket by time
	buckets := make(map[int64][]*tpackmodel.Trace)
	for _, t := range traces {
		bk := traceBucketKey(t, bucketDurationUs)
		buckets[bk] = append(buckets[bk], t)
	}

	return buckets, nil
}

// readOTLPChunked reads a directory of OTLP JSON chunk files one at a time.
func readOTLPChunked(dir string, bucketDurationUs int64, primaryAttributes, dependentAttributes []string) (map[int64][]*tpackmodel.Trace, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	pbFiles, _ := filepath.Glob(filepath.Join(dir, "*.pb"))
	files = append(files, pbFiles...)
	if len(files) == 0 {
		return nil, fmt.Errorf("no .json or .pb files in %s", dir)
	}
	sort.Strings(files)

	log.Printf("readOTLPChunked: reading %d chunk files from %s", len(files), dir)

	// Parallel read: each worker processes a shard of files.
	nw := max(min(runtime.NumCPU(), len(files)), 1)

	partials := make([]map[int64][]*tpackmodel.Trace, nw)
	var processed int64
	var processedMu sync.Mutex
	errCh := make(chan error, nw)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make(map[int64][]*tpackmodel.Trace)
			for i := w; i < len(files); i += nw {
				file := files[i]
				td, err := readOTLPFile(file)
				if err != nil {
					errCh <- fmt.Errorf("read %s: %w", file, err)
					return
				}
				traces := otlpconv.FromPdata(td, primaryAttributes, dependentAttributes)
				for _, t := range traces {
					bk := traceBucketKey(t, bucketDurationUs)
					local[bk] = append(local[bk], t)
				}
				processedMu.Lock()
				processed++
				p := processed
				processedMu.Unlock()
				if p%10 == 0 || int(p) == len(files) {
					log.Printf("readOTLPChunked: processed %d/%d chunks", p, len(files))
				}
			}
			partials[w] = local
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return nil, err
		}
	}

	// Merge per-bucket slices
	buckets := make(map[int64][]*tpackmodel.Trace)
	for _, local := range partials {
		for bk, ts := range local {
			buckets[bk] = append(buckets[bk], ts...)
		}
	}

	runtime.GC() // reclaim pdata
	return buckets, nil
}

// readOTLPFile reads a single OTLP file, handling both plain JSON and JSONL formats.
// It tries plain JSON first; if the file contains multiple lines, falls back to JSONL.
func readOTLPFile(file string) (ptrace.Traces, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return ptrace.Traces{}, fmt.Errorf("read %s: %w", file, err)
	}

	// Proto binary files
	if strings.HasSuffix(file, ".pb") {
		unmarshaler := &ptrace.ProtoUnmarshaler{}
		td, err := unmarshaler.UnmarshalTraces(data)
		if err != nil {
			return ptrace.Traces{}, fmt.Errorf("unmarshal pb %s: %w", file, err)
		}
		return td, nil
	}

	// Check if file has multiple lines (JSONL)
	if bytes.Count(data, []byte{'\n'}) > 1 {
		data = nil
		return readJSONLFile(file)
	}

	unmarshaler := &ptrace.JSONUnmarshaler{}
	td, err := unmarshaler.UnmarshalTraces(data)
	if err != nil {
		return ptrace.Traces{}, fmt.Errorf("unmarshal %s: %w", file, err)
	}
	return td, nil
}
