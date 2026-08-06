package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

type fieldSize struct {
	Name  string
	Bytes int
	Count int
}

// analyzeModelDir reads all gzipped model files in a compressed/data directory
// and prints aggregate per-field size breakdown.
func analyzeModelDir(dir string) error {
	dataDir := filepath.Join(dir, "compressed", "data")
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dataDir, err)
	}

	var modelFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "model_bucket_") {
			modelFiles = append(modelFiles, filepath.Join(dataDir, e.Name()))
		}
	}
	sort.Strings(modelFiles)

	if len(modelFiles) == 0 {
		return fmt.Errorf("no model_bucket_* files found in %s", dataDir)
	}

	log.Printf("Analyzing %d model files in %s", len(modelFiles), dataDir)

	// Aggregate across all buckets
	totals := make(map[string]int)
	counts := make(map[string]int)
	totalRaw := 0
	totalCompressed := int64(0)
	totalDeltaVocabEntries := 0
	totalCumulativeVocabSize := int32(0)
	deltaMode := false

	for _, path := range modelFiles {
		rawData, err := readGzipFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		totalRaw += len(rawData)

		fi, _ := os.Stat(path)
		totalCompressed += fi.Size()

		models := &pb.TPackModels{}
		if err := proto.Unmarshal(rawData, models); err != nil {
			return fmt.Errorf("unmarshal %s: %w", path, err)
		}

		if models.CumulativeVocabSize > 0 {
			deltaMode = true
			totalDeltaVocabEntries += len(models.NodeVocabulary)
			if models.CumulativeVocabSize > totalCumulativeVocabSize {
				totalCumulativeVocabSize = models.CumulativeVocabSize
			}
		}

		for _, f := range measureFields(models) {
			totals[f.Name] += f.Bytes
			counts[f.Name] += f.Count
		}
	}

	// Sort by size descending
	var fields []fieldSize
	for name, bytes := range totals {
		fields = append(fields, fieldSize{name, bytes, counts[name]})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Bytes > fields[j].Bytes })

	fmt.Printf("\n=== Model Size Analysis (%d buckets) ===\n", len(modelFiles))
	fmt.Printf("Total raw protobuf:  %d bytes (%.1f KB)\n", totalRaw, float64(totalRaw)/1024)
	fmt.Printf("Total gzip compressed: %d bytes (%.1f KB)\n", totalCompressed, float64(totalCompressed)/1024)
	fmt.Printf("Compression ratio:   %.2fx\n", float64(totalRaw)/float64(totalCompressed))
	fmt.Printf("Avg per bucket (raw): %d bytes\n", totalRaw/len(modelFiles))
	fmt.Printf("Avg per bucket (gz):  %d bytes\n\n", int(totalCompressed)/len(modelFiles))

	if deltaMode {
		fullVocabEquiv := int(totalCumulativeVocabSize) * len(modelFiles)
		fmt.Printf("Delta vocabulary encoding: ON\n")
		fmt.Printf("  Cumulative vocab size:   %d unique types\n", totalCumulativeVocabSize)
		fmt.Printf("  Total delta entries:     %d (vs %d full-vocab equivalent)\n", totalDeltaVocabEntries, fullVocabEquiv)
		fmt.Printf("  Delta reduction:         %.1f%%\n\n", (1-float64(totalDeltaVocabEntries)/float64(fullVocabEquiv))*100)
	}

	fmt.Printf("%-40s %10s %8s %8s %10s\n", "Field", "Total", "Avg/Bkt", "Count", "Pct")
	fmt.Printf("%-40s %10s %8s %8s %10s\n", strings.Repeat("-", 40), "-----", "-------", "-----", "---")
	for _, f := range fields {
		pct := float64(f.Bytes) / float64(totalRaw) * 100
		avg := f.Bytes / len(modelFiles)
		fmt.Printf("%-40s %10d %8d %8d %9.1f%%\n", f.Name, f.Bytes, avg, f.Count, pct)
	}
	fmt.Printf("%-40s %10d\n", "TOTAL RAW", totalRaw)

	return nil
}

func readGzipFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	return io.ReadAll(gz)
}

func measureFields(models *pb.TPackModels) []fieldSize {
	var fields []fieldSize

	measure := func(name string, count int, m proto.Message) {
		b, _ := proto.Marshal(m)
		if len(b) > 0 {
			fields = append(fields, fieldSize{name, len(b), count})
		}
	}

	if len(models.NodeVocabulary) > 0 {
		vocabLabel := "node_vocabulary"
		if models.CumulativeVocabSize > 0 {
			vocabLabel = fmt.Sprintf("node_vocabulary (delta, %d/%d entries)", len(models.NodeVocabulary), models.CumulativeVocabSize)
		}
		measure(vocabLabel, len(models.NodeVocabulary),
			&pb.TPackModels{NodeVocabulary: models.NodeVocabulary})
	}

	if models.Config != nil {
		measure("config", 1, &pb.TPackModels{Config: models.Config})
	}

	if models.MinStartTimeUs != 0 || models.MaxStartTimeUs != 0 || models.TraceCount != 0 {
		measure("timing_metadata", 1, &pb.TPackModels{
			MinStartTimeUs: models.MinStartTimeUs,
			MaxStartTimeUs: models.MaxStartTimeUs,
			TraceCount:     models.TraceCount,
		})
	}

	if len(models.RootModels) > 0 {
		measure("root_models", len(models.RootModels),
			&pb.TPackModels{RootModels: models.RootModels})
	}

	if len(models.TopologyModels) > 0 {
		measure("topology_models", len(models.TopologyModels),
			&pb.TPackModels{TopologyModels: models.TopologyModels})
	}

	if len(models.RootDurationModels) > 0 {
		measure("root_duration_models", len(models.RootDurationModels),
			&pb.TPackModels{RootDurationModels: models.RootDurationModels})
	}

	if len(models.SpanDurationBounds) > 0 {
		measure("span_duration_bounds", len(models.SpanDurationBounds),
			&pb.TPackModels{SpanDurationBounds: models.SpanDurationBounds})
	}

	if len(models.SpanGapBounds) > 0 {
		measure("span_gap_bounds", len(models.SpanGapBounds),
			&pb.TPackModels{SpanGapBounds: models.SpanGapBounds})
	}

	if len(models.StatisticalDependentAttributes) > 0 {
		measure("statistical_metadata (total)", len(models.StatisticalDependentAttributes),
			&pb.TPackModels{StatisticalDependentAttributes: models.StatisticalDependentAttributes})

		// Sub-breakdown
		totalRegression := 0
		totalMetaProbs := 0
		for _, sm := range models.StatisticalDependentAttributes {
			regOnly := &pb.StatisticalDependentAttributeModel{
				ParentNodeIdx: sm.ParentNodeIdx,
				ChildNodeIdx:  sm.ChildNodeIdx,
				SampleCount:   sm.SampleCount,
				GapBeta0: sm.GapBeta0,
				GapBeta1: sm.GapBeta1,
				DurBeta0: sm.DurBeta0,
				DurBeta1: sm.DurBeta1,
			}
			b, _ := proto.Marshal(regOnly)
			totalRegression += len(b)

			if len(sm.DependentAttributeProbs) > 0 {
				probsOnly := &pb.StatisticalDependentAttributeModel{
					DependentAttributeProbs: sm.DependentAttributeProbs,
				}
				b, _ := proto.Marshal(probsOnly)
				totalMetaProbs += len(b)
			}
		}
		fields = append(fields, fieldSize{"  ↳ regression_params", totalRegression, len(models.StatisticalDependentAttributes)})
		fields = append(fields, fieldSize{"  ↳ metadata_col_probs", totalMetaProbs, len(models.StatisticalDependentAttributes)})
	}

	if len(models.DependentAttributes) > 0 || len(models.DependentAttributeVocabs) > 0 {
		measure("dependent_attributes+vocabs", len(models.DependentAttributes),
			&pb.TPackModels{DependentAttributes: models.DependentAttributes, DependentAttributeVocabs: models.DependentAttributeVocabs})
	}

	if len(models.PrimaryAttributes) > 0 {
		measure("primary_attributes", len(models.PrimaryAttributes),
			&pb.TPackModels{PrimaryAttributes: models.PrimaryAttributes})
	}

	if models.MaxNodesCount > 0 {
		measure("max_nodes_count", 1,
			&pb.TPackModels{MaxNodesCount: models.MaxNodesCount})
	}

	return fields
}

