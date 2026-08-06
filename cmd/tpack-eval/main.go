package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"gopkg.in/yaml.v3"
)

// yamlConfig represents the YAML configuration file format.
type yamlConfig struct {
	Name              string   `yaml:"name"`
	PrimaryAttributes    []string `yaml:"primary_attributes"`
	DependentAttributes   []string `yaml:"dependent_attributes"`
	OffsetValue       string   `yaml:"offset_value"`
	OffsetModel       string   `yaml:"offset_model"`
	UseDurationBounds *bool    `yaml:"use_duration_bounds"` // nil = default (true)
	TopologyMode      string   `yaml:"topology_mode"`       // "edge" (default) or "template"
	MaxGMMComponents  *int32   `yaml:"max_gmm_components"`  // nil = default (3)
}

func main() {
	inputPath := flag.String("input", "", "Path to input traces (OTLP JSON or proto binary)")
	outputDir := flag.String("output", "", "Output dataset directory")
	bucketSec := flag.Int("bucket", 60, "Time bucket duration in seconds")
	workers := flag.Int("workers", runtime.NumCPU(), "Parallel goroutines")
	seed := flag.Int("seed", 42, "Random seed")
	headSample := flag.Bool("head-sample", false, "Run head sampling mode instead of TPack compression")
	tailSample := flag.Bool("tail-sample", false, "Run tail sampling (error+latency biased, full transmission cost)")
	hindsightSample := flag.Bool("hindsight-sample", false, "Run hindsight sampling (error+latency biased, subset transmission cost)")
	sifterSample := flag.Bool("sifter-sample", false, "Run Sifter biased sampling (paragraph vector model)")
	samplingRates := flag.String("sampling-rates", "", "Comma-separated sampling rates for head sampling (e.g. 2,10,150)")
	iterations := flag.Int("iterations", 3, "Number of iterations per sampling rate")
	evaluateOnly := flag.Bool("evaluate-only", false, "Only run evaluators on an existing dataset (--input is the OTLP file)")
	primaryAttributesFlag := flag.String("primary-attributes",
		"service.name,span.kind,operation.name,status.code",
		"Comma-separated columns that form node identity")
	dependentAttributesFlag := flag.String("dependent-attributes", "", "Comma-separated metadata columns to model (e.g. http.status_code)")
	analyzeMetadata := flag.Bool("analyze-attributes", false, "Analyze metadata η² effect on duration ratios (then exit)")
	minCoverage := flag.Float64("min-coverage", 0.1, "Minimum attribute coverage (0-1) for auto-discovery in --analyze-attributes")
	analyzeModel := flag.Bool("analyze-model", false, "Analyze model size breakdown from compressed output directory (then exit)")
	transform := flag.Bool("transform", false, "Transform OTLP JSONL/CSV/Jaeger directory to OTLP JSON (then exit)")
	remapFlag := flag.Bool("remap", false, "Remap timestamps to [0,60s), discard traces with root duration > 60s")
	maxTraces := flag.Int("max-traces", 0, "Max traces to include in transform (0 = all)")
	maxSpansPerChunk := flag.Int("max-spans-per-chunk", 0, "Max spans per chunk file during transform (0 = unlimited)")
	report := flag.Bool("report", false, "Generate evaluation report (replaces Python generate_report)")
	compressorPrefixes := flag.String("compressors", "head,tpack,tail,hindsight,sifter", "Comma-separated compressor prefixes for --report")
	flattenCSV := flag.Bool("flatten-csv", false, "Flatten OTLP traces to a CSV file with one row per span (then exit)")
	datasetStats := flag.Bool("dataset-stats", false, "Collect dataset statistics (spans, traces, services) and exit")
	statsOutputJSON := flag.String("stats-output-json", "", "Write dataset stats as JSON to this path")
	statsSections := flag.String("stats-sections", "basic,columns,insights,templates", "Comma-separated sections to compute: basic,columns,insights,templates")
	analyzeOffsets := flag.Bool("analyze-offsets", false, "Compare ratio vs log-offset duration models (then exit)")
	failureAnalysis := flag.Bool("failure-analysis", false, "Run failure case analysis comparing approaches to baseline")
	baselineFlag := flag.String("baseline", "head_100", "Baseline compressor prefix for --failure-analysis")
	featureSetsFlag := flag.String("feature-sets", "", "Per-approach feature columns: prefix=col1,col2;prefix2=col3,col4")
	traceInput := flag.String("trace-input", "", "Path to original trace data directory (for --failure-analysis avg_depth/shared_edge_rate)")
	configFile := flag.String("config", "", "Path to YAML config file (overrides --primary-attributes, --dependent-attributes)")
	offsetValueFlag := flag.String("offset-value", "ratio", "Offset value space: ratio or absolute")
	offsetModelFlag := flag.String("offset-model", "regression", "Offset distribution model: regression or percentile")
	cpuProfile := flag.String("cpuprofile", "", "Write CPU profile to file")
	traceProfile := flag.String("trace", "", "Write execution trace to file (view with: go tool trace <file>)")
	skipOutput := flag.Bool("skip-output", false, "Skip writing OTLP output (only write timing + model files)")
	gzipSize := flag.Bool("gzip-size", false, "Compute gzip-compressed size of input directory (then exit)")
	gzipBench := flag.Bool("gzip-bench", false, "Benchmark gzip compress+decompress on input directory (then exit)")
	injectTimePath := flag.String("inject-time", "", "Path to inject_time.txt (Unix epoch seconds, filters RCA to post-injection spans)")
	flag.Parse()

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatalf("Could not create CPU profile: %v", err)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *traceProfile != "" {
		f, err := os.Create(*traceProfile)
		if err != nil {
			log.Fatalf("Could not create trace file: %v", err)
		}
		defer f.Close()
		trace.Start(f)
		defer trace.Stop()
	}

	// Ablation flags from YAML (nil = use default)
	var cfgUseDurationBounds *bool
	var cfgTopologyMode string
	var cfgMaxGMMComponents *int32

	// Load YAML config if provided
	if *configFile != "" {
		data, err := os.ReadFile(*configFile)
		if err != nil {
			log.Fatalf("Failed to read config file %s: %v", *configFile, err)
		}
		var cfg yamlConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			log.Fatalf("Failed to parse config file %s: %v", *configFile, err)
		}
		log.Printf("Loaded config: %s", cfg.Name)

		// Config sets defaults; CLI flags can still override
		if len(cfg.PrimaryAttributes) > 0 && !isFlagSet("primary-attributes") {
			*primaryAttributesFlag = strings.Join(cfg.PrimaryAttributes, ",")
		}
		if len(cfg.DependentAttributes) > 0 && !isFlagSet("dependent-attributes") {
			*dependentAttributesFlag = strings.Join(cfg.DependentAttributes, ",")
		}
		if cfg.OffsetValue != "" && !isFlagSet("offset-value") {
			*offsetValueFlag = cfg.OffsetValue
		}
		if cfg.OffsetModel != "" && !isFlagSet("offset-model") {
			*offsetModelFlag = cfg.OffsetModel
		}
		cfgUseDurationBounds = cfg.UseDurationBounds
		cfgTopologyMode = cfg.TopologyMode
		cfgMaxGMMComponents = cfg.MaxGMMComponents
	}

	// Validate offset config
	if *offsetValueFlag != "ratio" && *offsetValueFlag != "absolute" {
		log.Fatalf("--offset-value must be 'ratio' or 'absolute', got %q", *offsetValueFlag)
	}
	if *offsetModelFlag != "regression" && *offsetModelFlag != "percentile" {
		log.Fatalf("--offset-model must be 'regression' or 'percentile', got %q", *offsetModelFlag)
	}

	// Parse feature columns
	var primaryAttributes []string
	for col := range strings.SplitSeq(*primaryAttributesFlag, ",") {
		col = strings.TrimSpace(col)
		if col != "" {
			primaryAttributes = append(primaryAttributes, col)
		}
	}

	// Parse metadata columns, auto-excluding any that are also feature columns
	featureSet := make(map[string]bool, len(primaryAttributes))
	for _, col := range primaryAttributes {
		featureSet[col] = true
	}
	var dependentAttributes []string
	if *dependentAttributesFlag != "" {
		for col := range strings.SplitSeq(*dependentAttributesFlag, ",") {
			col = strings.TrimSpace(col)
			if col != "" {
				if featureSet[col] {
					log.Printf("Warning: metadata column %q is also a feature column; excluding from metadata", col)
					continue
				}
				dependentAttributes = append(dependentAttributes, col)
			}
		}
	}

	// --gzip-size: compute gzip-compressed size of input directory
	if *gzipSize {
		if *inputPath == "" {
			log.Fatalf("--gzip-size requires --input")
		}
		size, err := computeGzipSize(*inputPath, *workers)
		if err != nil {
			log.Fatalf("gzip-size failed: %v", err)
		}
		fmt.Println(size)
		return
	}

	// --gzip-bench: benchmark gzip compress + decompress
	if *gzipBench {
		if *inputPath == "" {
			log.Fatalf("--gzip-bench requires --input")
		}
		r, err := benchmarkGzip(*inputPath, *workers, true, "")
		if err != nil {
			log.Fatalf("gzip-bench failed: %v", err)
		}
		fmt.Printf("%d %.2f %.2f\n", r.CompressedSize, r.CompressSeconds, r.DecompressSeconds)
		return
	}

	// --flatten-csv: flatten OTLP to CSV (early exit)
	if *flattenCSV {
		if *inputPath == "" || *outputDir == "" {
			log.Fatalf("--flatten-csv requires --input and --output")
		}
		bdu := int64(*bucketSec) * 1_000_000
		buckets, err := readOTLP(*inputPath, bdu, primaryAttributes, dependentAttributes)
		if err != nil {
			log.Fatalf("Failed to read input: %v", err)
		}
		if err := writeFlattenedCSV(*outputDir, buckets, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Failed to write CSV: %v", err)
		}
		return
	}

	// --dataset-stats: collect dataset statistics (early exit)
	if *datasetStats {
		if *inputPath == "" {
			log.Fatalf("--dataset-stats requires --input")
		}
		if err := runDatasetStats(*inputPath, *workers, *statsOutputJSON, primaryAttributes, *statsSections); err != nil {
			log.Fatalf("Dataset stats failed: %v", err)
		}
		return
	}

	// --report: generate evaluation report (early exit)
	if *report {
		if *outputDir == "" {
			log.Fatalf("--report requires --output (path to output report.json)")
		}
		prefixes := strings.Split(*compressorPrefixes, ",")
		if err := generateReport(*outputDir, prefixes); err != nil {
			log.Fatalf("Report generation failed: %v", err)
		}
		return
	}

	// --failure-analysis: compare approaches to baseline (early exit)
	if *failureAnalysis {
		if *inputPath == "" {
			log.Fatalf("--failure-analysis requires --input (path to report.json)")
		}
		prefixes := strings.Split(*compressorPrefixes, ",")
		featureSetsMap := parseFeatureSets(*featureSetsFlag)
		if err := runFailureAnalysis(*inputPath, prefixes, *baselineFlag, featureSetsMap, *traceInput); err != nil {
			log.Fatalf("Failure analysis failed: %v", err)
		}
		return
	}

	// --analyze-model: analyze model size breakdown
	if *analyzeModel {
		if *outputDir == "" {
			log.Fatalf("--analyze-model requires --output (compressor output directory)")
		}
		if err := analyzeModelDir(*outputDir); err != nil {
			log.Fatalf("Analyze model failed: %v", err)
		}
		return
	}

	// --analyze-attributes: early exit
	if *analyzeMetadata {
		if *inputPath == "" {
			log.Fatalf("--analyze-attributes requires --input")
		}
		if len(dependentAttributes) == 0 {
			// Auto-discover metadata columns from the data
			log.Printf("No --dependent-attributes specified; auto-discovering attributes with ≥%.0f%% coverage ...", *minCoverage*100)
			discovered, err := discoverDependentAttributes(*inputPath, primaryAttributes, *minCoverage)
			if err != nil {
				log.Fatalf("Auto-discovery failed: %v", err)
			}
			if len(discovered) == 0 {
				log.Fatalf("No attributes found with ≥%.0f%% coverage", *minCoverage*100)
			}
			dependentAttributes = discovered
			log.Printf("Discovered %d columns: %v", len(dependentAttributes), dependentAttributes)
		}
		bucketDurationUs := int64(*bucketSec) * 1_000_000
		if err := runAnalyzeAttributes(*inputPath, bucketDurationUs, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Analyze metadata failed: %v", err)
		}
		return
	}

	// --analyze-offsets: compare ratio vs log-offset duration models (early exit)
	if *analyzeOffsets {
		if *inputPath == "" {
			log.Fatalf("--analyze-offsets requires --input")
		}
		bucketDurationUs := int64(*bucketSec) * 1_000_000
		if err := runAnalyzeOffsets(*inputPath, bucketDurationUs, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Analyze offsets failed: %v", err)
		}
		return
	}

	bucketDurationUs := int64(*bucketSec) * 1_000_000

	// --transform: early exit
	if *transform {
		if *inputPath == "" || *outputDir == "" {
			log.Fatalf("--transform requires --input (directory or .csv) and --output (directory)")
		}
		if err := runTransform(*inputPath, *outputDir, primaryAttributes, dependentAttributes, *maxTraces, *maxSpansPerChunk, *remapFlag, bucketDurationUs); err != nil {
			log.Fatalf("Transform failed: %v", err)
		}
		return
	}

	if *inputPath == "" || *outputDir == "" {
		fmt.Fprintf(os.Stderr, "Usage: tpack-eval --input traces.json --output output/dir/\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Head sampling mode
	if *headSample {
		if *samplingRates == "" {
			log.Fatalf("--sampling-rates is required with --head-sample")
		}
		rates, err := parseRates(*samplingRates)
		if err != nil {
			log.Fatalf("Invalid --sampling-rates: %v", err)
		}
		if err := runHeadSampling(*inputPath, *outputDir, rates, *iterations, int64(*seed), bucketDurationUs, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Head sampling failed: %v", err)
		}
		return
	}

	// Tail sampling mode
	if *tailSample {
		if err := runTailSampling(*inputPath, *outputDir, bucketDurationUs, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Tail sampling failed: %v", err)
		}
		return
	}

	// Hindsight sampling mode
	if *hindsightSample {
		if err := runHindsightSampling(*inputPath, *outputDir, bucketDurationUs, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Hindsight sampling failed: %v", err)
		}
		return
	}

	// Sifter sampling mode
	if *sifterSample {
		if *samplingRates == "" {
			log.Fatalf("--sampling-rates is required with --sifter-sample")
		}
		rates, err := parseRates(*samplingRates)
		if err != nil {
			log.Fatalf("Invalid --sampling-rates: %v", err)
		}
		if err := runSifterSampling(*inputPath, *outputDir, rates, *iterations, int64(*seed), bucketDurationUs, primaryAttributes, dependentAttributes); err != nil {
			log.Fatalf("Sifter sampling failed: %v", err)
		}
		return
	}

	// Evaluate-only mode: read existing dataset and run evaluators
	if *evaluateOnly {
		log.Printf("Evaluate-only mode: reading %s ...", *inputPath)
		t0 := time.Now()
		buckets, err := readOTLP(*inputPath, bucketDurationUs, primaryAttributes, dependentAttributes)
		if err != nil {
			log.Fatalf("Failed to read input: %v", err)
		}

		totalTraces := 0
		totalSpans := 0
		for _, traces := range buckets {
			totalTraces += len(traces)
			for _, t := range traces {
				totalSpans += len(t.Spans)
			}
		}
		log.Printf("Read %d traces (%d spans) in %d buckets [%.1fs]",
			totalTraces, totalSpans, len(buckets), time.Since(t0).Seconds())

		var injectTimeUs *int64
		if *injectTimePath != "" {
			data, err := os.ReadFile(*injectTimePath)
			if err != nil {
				log.Fatalf("Failed to read inject-time file: %v", err)
			}
			epochSec, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				log.Fatalf("Failed to parse inject-time: %v", err)
			}
			us := epochSec * 1_000_000
			injectTimeUs = &us
			log.Printf("Inject time: %d seconds (filtering RCA to post-injection spans)", epochSec)
		}

		traces := originalToEvalTraces(buckets)
		if err := runAllEvaluators(*outputDir, traces, injectTimeUs, 0, 0); err != nil {
			log.Fatalf("Failed to evaluate: %v", err)
		}
		log.Printf("Evaluate-only complete [%.1fs]", time.Since(t0).Seconds())
		return
	}

	log.Printf("Feature columns: %v", primaryAttributes)
	if len(dependentAttributes) > 0 {
		log.Printf("Metadata columns: %v", dependentAttributes)
	}

	config := tpackmodel.DefaultConfig()
	config.RandomSeed = int32(*seed)
	config.OffsetValue = *offsetValueFlag
	config.OffsetModel = *offsetModelFlag
	if cfgUseDurationBounds != nil {
		config.UseDurationBounds = *cfgUseDurationBounds
	}
	if cfgTopologyMode != "" {
		config.TopologyMode = cfgTopologyMode
	}
	if cfgMaxGMMComponents != nil {
		config.MaxGMMComponents = *cfgMaxGMMComponents
	}
	log.Printf("Offset: value=%s, model=%s, useBounds=%v, topologyMode=%s, maxGMM=%d",
		config.OffsetValue, config.OffsetModel, config.UseDurationBounds,
		config.TopologyMode, config.MaxGMMComponents)

	bucketKeys, results, err := processChunkedStreaming(*inputPath, bucketDurationUs, config, primaryAttributes, dependentAttributes, *skipOutput, *workers)
	if err != nil {
		log.Fatalf("Failed to process: %v", err)
	}

	var decompressSeconds float64
	for _, r := range results {
		decompressSeconds += r.GenerateSeconds
	}

	// Use compute wall time (excludes I/O) for cost model
	computeWall := 0.0
	if len(results) > 0 {
		computeWall = results[0].ComputeWallSeconds
	}

	totalInputTraces := 0
	totalInputSpans := 0
	for _, r := range results {
		totalInputTraces += r.InputTraces
		totalInputSpans += r.InputSpans
	}

	// Step 3: Write output (timing + model first, then large OTLP)
	log.Printf("Writing output to %s ...", *outputDir)
	if err := writeCompressedData(*outputDir, results, computeWall, decompressSeconds, totalInputTraces, totalInputSpans); err != nil {
		log.Fatalf("Failed to write compressed data: %v", err)
	}

	if !*skipOutput {
		if err := writeOutputOTLP(*outputDir, bucketKeys, results); err != nil {
			log.Fatalf("Failed to write OTLP: %v", err)
		}
	}
}

// parseFeatureSets parses "prefix=col1,col2;prefix2=col3" into a map of prefix → set of column names.
func parseFeatureSets(s string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	if s == "" {
		return result
	}
	for entry := range strings.SplitSeq(s, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			log.Printf("Warning: invalid feature-set entry %q (expected prefix=col1,col2)", entry)
			continue
		}
		prefix := strings.TrimSpace(parts[0])
		cols := make(map[string]bool)
		for col := range strings.SplitSeq(parts[1], ",") {
			col = strings.TrimSpace(col)
			if col != "" {
				cols[col] = true
			}
		}
		result[prefix] = cols
	}
	return result
}

// writeCompressedData writes per-bucket serialized models for cost calculation.
func writeCompressedData(outputDir string, results []bucketResult, cpuSeconds, decompressSeconds float64, inputTraces, inputSpans int) error {
	compressorDir := filepath.Dir(filepath.Clean(outputDir))
	dataDir := filepath.Join(compressorDir, "compressed", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	var gzCompressSeconds, gzDecompressSeconds float64
	var totalModelRawBytes int
	for _, r := range results {
		totalModelRawBytes += len(r.ModelBytes)
		// Gzip compress (timed, in-memory only)
		tGz := time.Now()
		var buf bytes.Buffer
		gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		gz.Write(r.ModelBytes)
		gz.Close()
		gzCompressSeconds += time.Since(tGz).Seconds()

		compressed := buf.Bytes()

		// Gzip decompress benchmark (timed, in-memory only)
		tGzD := time.Now()
		gr, _ := gzip.NewReader(bytes.NewReader(compressed))
		io.Copy(io.Discard, gr)
		gr.Close()
		gzDecompressSeconds += time.Since(tGzD).Seconds()

		// Write to disk (not timed)
		path := filepath.Join(dataDir, fmt.Sprintf("model_bucket_%d", r.BucketKey))
		if err := os.WriteFile(path, compressed, 0o644); err != nil {
			return fmt.Errorf("write model bucket %d: %w", r.BucketKey, err)
		}
	}

	// TPack: all compute is CPU (train + generate + gzip)
	trainSeconds := cpuSeconds - decompressSeconds
	compCPU := trainSeconds + gzCompressSeconds
	decompCPU := decompressSeconds + gzDecompressSeconds

	log.Printf("Gzip timing: compress %.3fs, decompress %.3fs", gzCompressSeconds, gzDecompressSeconds)
	log.Printf("Time report (written to %s):", dataDir)
	log.Printf("  Compression:   %.1fs (train %.1fs + gzip %.1fs)", compCPU, trainSeconds, gzCompressSeconds)
	log.Printf("  Decompression: %.1fs (generate %.1fs + gzip %.1fs)", decompCPU, decompressSeconds, gzDecompressSeconds)

	if err := writeTimingFiles(dataDir, compCPU, 0, decompCPU, 0, inputTraces, inputSpans); err != nil {
		return err
	}
	// Write uncompressed model size
	if err := os.WriteFile(filepath.Join(dataDir, "model_raw_bytes"), fmt.Appendf(nil, "%d", totalModelRawBytes), 0o644); err != nil {
		return err
	}

	return nil
}

// writeTimingFiles writes 4 canonical timing files to the given directory:
// compression_cpu_time_seconds, compression_gpu_time_seconds,
// decompression_cpu_time_seconds, decompression_gpu_time_seconds.
func writeTimingFiles(dir string, compCPU, compGPU, decompCPU, decompGPU float64, inputTraces, inputSpans int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	for name, val := range map[string]float64{
		"compression_cpu_time_seconds":   compCPU,
		"compression_gpu_time_seconds":   compGPU,
		"decompression_cpu_time_seconds": decompCPU,
		"decompression_gpu_time_seconds": decompGPU,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, fmt.Appendf(nil, "%.6f", val), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	for name, val := range map[string]int{
		"input_traces": inputTraces,
		"input_spans":  inputSpans,
	} {
		if val > 0 {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, fmt.Appendf(nil, "%d", val), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		}
	}

	return nil
}

// processChunkedStreaming reads chunk files in batches and trains a single
// streaming model across all chunks. Only one batch of traces is in memory at a time.
func processChunkedStreaming(inputPath string, bucketDurationUs int64, config tpackmodel.TPackConfig, primaryAttributes, dependentAttributes []string, discardSpans bool, numWorkersFlag int) ([]int64, []bucketResult, error) {
	const chunksPerBatch = 200

	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", inputPath, err)
	}

	var files []string
	if info.IsDir() {
		jsonFiles, _ := filepath.Glob(filepath.Join(inputPath, "*.json"))
		pbFiles, _ := filepath.Glob(filepath.Join(inputPath, "*.pb"))
		files = append(jsonFiles, pbFiles...)
		if len(files) == 0 {
			return nil, nil, fmt.Errorf("no .json or .pb files in %s", inputPath)
		}
		sort.Strings(files)
	} else {
		files = []string{inputPath}
	}

	numWorkers := min(numWorkersFlag, len(files))

	// Split files into batches
	var batches [][]string
	for i := 0; i < len(files); i += chunksPerBatch {
		end := min(i+chunksPerBatch, len(files))
		batches = append(batches, files[i:end])
	}
	log.Printf("Batched training: %d chunks in %d batches (%d chunks/batch, %d workers) in %s",
		len(files), len(batches), chunksPerBatch, numWorkers, inputPath)

	// Accumulate per-bucket trainers across all batches
	globalTrainers := make(map[int64]*tpackmodel.StreamingTrainer)
	var mu sync.Mutex

	totalTraces := 0
	totalSpans := 0
	var totalIOSecs, totalComputeSecs float64

	for bi, batch := range batches {
		batchWorkers := min(numWorkers, len(batch))

		// Distribute batch files round-robin to workers
		workerFiles := make([][]string, batchWorkers)
		for i, f := range batch {
			workerFiles[i%batchWorkers] = append(workerFiles[i%batchWorkers], f)
		}

		// ── I/O phase: read batch chunks in parallel ──
		type workerData struct {
			bucketedTraces map[int64][]*tpackmodel.Trace
			traces         int
			spans          int
		}
		ioData := make([]workerData, batchWorkers)
		var wg sync.WaitGroup

		tIO := time.Now()
		for w := range batchWorkers {
			ioData[w] = workerData{bucketedTraces: make(map[int64][]*tpackmodel.Trace)}
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for _, file := range workerFiles[w] {
					td, err := readOTLPFile(file)
					if err != nil {
						log.Fatalf("read %s: %v", file, err)
					}
					traces := otlpconv.FromPdata(td, primaryAttributes, dependentAttributes)
					for _, t := range traces {
						bk := traceBucketKey(t, bucketDurationUs)
						ioData[w].bucketedTraces[bk] = append(ioData[w].bucketedTraces[bk], t)
						ioData[w].spans += len(t.Spans)
					}
					ioData[w].traces += len(traces)
				}
			}(w)
		}
		wg.Wait()
		totalIOSecs += time.Since(tIO).Seconds()

		batchTraces := 0
		batchSpans := 0
		for w := range batchWorkers {
			batchTraces += ioData[w].traces
			batchSpans += ioData[w].spans
		}
		totalTraces += batchTraces
		totalSpans += batchSpans

		// ── Compute phase: train on batch, merge into global trainers, discard traces ──
		tCompute := time.Now()
		for w := range batchWorkers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for bk, traces := range ioData[w].bucketedTraces {
					st := tpackmodel.NewStreamingTrainer(config, primaryAttributes, dependentAttributes)
					for _, t := range traces {
						st.AddTrace(t)
					}
					mu.Lock()
					if existing, ok := globalTrainers[bk]; ok {
						globalTrainers[bk] = tpackmodel.MergeTrainers([]*tpackmodel.StreamingTrainer{existing, st})
					} else {
						globalTrainers[bk] = st
					}
					mu.Unlock()
				}
				ioData[w].bucketedTraces = nil // free memory
			}(w)
		}
		wg.Wait()
		totalComputeSecs += time.Since(tCompute).Seconds()

		log.Printf("  Batch %d/%d: %d traces (%d spans), I/O %.1fs, train %.1fs",
			bi+1, len(batches), batchTraces, batchSpans,
			time.Since(tIO).Seconds()-time.Since(tCompute).Seconds(),
			time.Since(tCompute).Seconds())
	}

	// ── Final compute: finalize and generate ──
	tFinal := time.Now()

	bucketKeys := make([]int64, 0, len(globalTrainers))
	for bk := range globalTrainers {
		bucketKeys = append(bucketKeys, bk)
	}
	slices.Sort(bucketKeys)

	results := make([]bucketResult, len(bucketKeys))
	for i, bk := range bucketKeys {
		log.Printf("  Bucket %d (%d/%d): finalizing...", bk, i+1, len(bucketKeys))

		tFinalize := time.Now()
		state := globalTrainers[bk].Finalize()
		finalizeSec := time.Since(tFinalize).Seconds()

		tGen := time.Now()
		spans, spanCount := tpackmodel.GenerateBucket(state, tpackmodel.GenerateOptions{
			BucketKey:    bk,
			DiscardSpans: discardSpans,
		})
		genSec := time.Since(tGen).Seconds()

		tMarshal := time.Now()
		modelBytes, _ := state.Marshal()
		marshalSec := time.Since(tMarshal).Seconds()

		results[i] = bucketResult{
			BucketKey:       bk,
			Spans:           spans,
			SpanCount:       spanCount,
			ModelBytes:      modelBytes,
			GenerateSeconds: genSec,
			Encoder:         state.NodeEncoder,
		}
		log.Printf("  Bucket %d (%d/%d): %d spans, %d bytes (finalize %.1fs, generate %.1fs, marshal %.1fs)",
			bk, i+1, len(bucketKeys), spanCount, len(modelBytes), finalizeSec, genSec, marshalSec)
	}

	totalComputeSecs += time.Since(tFinal).Seconds()
	var totalGenSecs float64
	for _, r := range results {
		totalGenSecs += r.GenerateSeconds
	}

	log.Printf("Summary: %d traces (%d spans) in %d buckets", totalTraces, totalSpans, len(bucketKeys))
	log.Printf("  I/O time:          %.1fs", totalIOSecs)
	log.Printf("  Compute time:      %.1fs (compression: %.1fs, decompression: %.1fs)",
		totalComputeSecs, totalComputeSecs-totalGenSecs, totalGenSecs)

	if len(results) > 0 {
		results[0].InputTraces = totalTraces
		results[0].InputSpans = totalSpans
		results[0].IOWallSeconds = totalIOSecs
		results[0].ComputeWallSeconds = totalComputeSecs
	}

	return bucketKeys, results, nil
}

// isFlagSet returns true if the named flag was explicitly set on the command line.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
