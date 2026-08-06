package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// runOtelArrowSampling produces otelarrow_{rate}_{iter}/ directories that
// share head sampling's dataset and evaluation output (via symlinks) but
// record a different cost: the bytes an upstream otelarrowexporter would
// have put on the wire (Arrow IPC + zstd, serialized as BatchArrowRecords),
// plus actual encode/decode wall time. Fidelity therefore matches head at
// the same rate by construction.
//
// Expects headBaseDir/head_{rate}_{iter}/dataset/ to already exist — head
// sampling must run first.
func runOtelArrowSampling(baseOutputDir string, rates []int, iterations int) error {
	for _, rate := range rates {
		for iter := 1; iter <= iterations; iter++ {
			headDir := fmt.Sprintf("head_%d_%d", rate, iter)
			arrowDir := fmt.Sprintf("otelarrow_%d_%d", rate, iter)

			headRoot := filepath.Join(baseOutputDir, headDir)
			headDataset := filepath.Join(headRoot, "dataset")
			if _, err := os.Stat(headDataset); err != nil {
				return fmt.Errorf("head dataset missing (%s): run --head-sample first: %w", headDataset, err)
			}

			outRoot := filepath.Join(baseOutputDir, arrowDir)
			outDataset := filepath.Join(outRoot, "dataset")
			outEvaluated := filepath.Join(outRoot, "evaluated")
			compressedDir := filepath.Join(outRoot, "compressed", "data")

			if err := os.MkdirAll(outRoot, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", outRoot, err)
			}

			// Symlink dataset/ → ../head_{rate}_{iter}/dataset and evaluated/ → .../evaluated.
			// Relative targets keep the tree relocatable (matches materializeRate1's idiom).
			if err := ensureRelSymlink(outDataset, "../"+headDir+"/dataset"); err != nil {
				return err
			}
			if err := ensureRelSymlink(outEvaluated, "../"+headDir+"/evaluated"); err != nil {
				return err
			}

			log.Printf("otel-arrow rate=1/%d iteration=%d → %s", rate, iter, arrowDir)

			result, err := benchmarkOtelArrow(headDataset, true, compressedDir)
			if err != nil {
				return fmt.Errorf("arrow benchmark %s: %w", arrowDir, err)
			}
			if err := writeTimingFiles(compressedDir, result.EncodeSeconds, 0, result.DecodeSeconds, 0, 0, 0); err != nil {
				return fmt.Errorf("write timing %s: %w", arrowDir, err)
			}
		}
	}
	return nil
}

// ensureRelSymlink creates a symlink at path pointing to target, replacing
// any stale real directory (idempotent across reruns).
func ensureRelSymlink(path, target string) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove stale %s: %w", path, err)
		}
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", path, target, err)
	}
	return nil
}
