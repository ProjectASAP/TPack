package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// GzipResult holds the results of gzip benchmarking.
type GzipResult struct {
	CompressedSize    int64
	CompressSeconds   float64
	DecompressSeconds float64
}

// computeGzipSize streams all .json files in dir through gzip.BestCompression
// using multiple workers and returns the total compressed size.
func computeGzipSize(dir string, numWorkers int) (int64, error) {
	r, err := benchmarkGzip(dir, numWorkers, false, "")
	if err != nil {
		return 0, err
	}
	return r.CompressedSize, nil
}

// benchmarkGzip compresses all .json/.pb files in dir using parallel workers.
// If decompress is true, also benchmarks decompression.
// If writeDir is non-empty, the gzipped output is written to writeDir as
// model_bucket_{idx} files (so samplers can avoid re-gzipping for cost accounting).
// Processes files in batches to limit memory usage.
func benchmarkGzip(dir string, numWorkers int, decompress bool, writeDir string) (GzipResult, error) {
	const batchSize = 200

	jsonFiles, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	pbFiles, _ := filepath.Glob(filepath.Join(dir, "*.pb"))
	files := append(jsonFiles, pbFiles...)
	if len(files) == 0 {
		return GzipResult{}, fmt.Errorf("no .json or .pb files in %s", dir)
	}
	sort.Strings(files)

	if numWorkers <= 0 {
		numWorkers = len(files)
	}
	if numWorkers > 16 {
		numWorkers = 16
	}

	fmt.Fprintf(os.Stderr, "  gzip benchmark: %d files in batches of %d\n", len(files), batchSize)

	if writeDir != "" {
		if err := os.MkdirAll(writeDir, 0o755); err != nil {
			return GzipResult{}, fmt.Errorf("mkdir %s: %w", writeDir, err)
		}
	}

	var totalSize atomic.Int64
	var totalCompSecs, totalDecompSecs float64
	totalFiles := int64(len(files))
	var filesDone atomic.Int64

	for batchStart := 0; batchStart < len(files); batchStart += batchSize {
		batchEnd := min(batchStart+batchSize, len(files))
		batch := files[batchStart:batchEnd]

		batchWorkers := min(numWorkers, len(batch))

		// Pre-read batch into memory
		batchContents := make([][]byte, len(batch))
		for i, f := range batch {
			data, err := os.ReadFile(f)
			if err != nil {
				return GzipResult{}, fmt.Errorf("read %s: %w", f, err)
			}
			batchContents[i] = data
		}

		// Distribute batch indices to workers
		workerIndices := make([][]int, batchWorkers)
		for i := range batch {
			workerIndices[i%batchWorkers] = append(workerIndices[i%batchWorkers], i)
		}

		compressedBuffers := make([][]byte, len(batch))
		var wg sync.WaitGroup
		var firstErr error
		var errOnce sync.Once

		// Compress
		compStart := time.Now()
		for w := range batchWorkers {
			wg.Add(1)
			go func(indices []int) {
				defer wg.Done()
				for _, idx := range indices {
					var buf bytes.Buffer
					gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
					if _, err := gz.Write(batchContents[idx]); err != nil {
						errOnce.Do(func() { firstErr = err })
						return
					}
					gz.Close()
					totalSize.Add(int64(buf.Len()))
					if decompress {
						compressedBuffers[idx] = buf.Bytes()
					}
					// Persist gzipped bytes for cost accounting if requested.
					// Done outside the timing measurement would be ideal but adding
					// it inline is negligible compared to the gzip work itself.
					if writeDir != "" {
						fileIdx := batchStart + idx
						path := filepath.Join(writeDir, fmt.Sprintf("model_bucket_%d", fileIdx))
						if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
							errOnce.Do(func() { firstErr = err })
							return
						}
					}
					done := filesDone.Add(1)
					if done%100 == 0 || done == totalFiles {
						fmt.Fprintf(os.Stderr, "\r  gzip compress: %d/%d files", done, totalFiles)
					}
				}
			}(workerIndices[w])
		}
		wg.Wait()
		totalCompSecs += time.Since(compStart).Seconds()

		if firstErr != nil {
			return GzipResult{}, firstErr
		}

		// Decompress
		if decompress {
			decompStart := time.Now()
			for w := range batchWorkers {
				wg.Add(1)
				go func(indices []int) {
					defer wg.Done()
					for _, idx := range indices {
						gr, err := gzip.NewReader(bytes.NewReader(compressedBuffers[idx]))
						if err != nil {
							errOnce.Do(func() { firstErr = err })
							return
						}
						io.Copy(io.Discard, gr)
						gr.Close()
					}
				}(workerIndices[w])
			}
			wg.Wait()
			totalDecompSecs += time.Since(decompStart).Seconds()

			if firstErr != nil {
				return GzipResult{}, firstErr
			}
		}

		// Free batch memory
		batchContents = nil
		compressedBuffers = nil
	}

	fmt.Fprintf(os.Stderr, "\r  gzip compress: %d/%d files (%.1fs)\n", totalFiles, totalFiles, totalCompSecs)
	if decompress {
		fmt.Fprintf(os.Stderr, "  gzip decompress: %d/%d files (%.1fs)\n", totalFiles, totalFiles, totalDecompSecs)
	}

	return GzipResult{
		CompressedSize:    totalSize.Load(),
		CompressSeconds:   totalCompSecs,
		DecompressSeconds: totalDecompSecs,
	}, nil
}

// countingWriter counts bytes written to it without storing them.
type countingWriter struct {
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
