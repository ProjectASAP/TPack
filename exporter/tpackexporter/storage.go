package tpackexporter

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Storage defines the interface for storing compressed model data.
type Storage interface {
	Store(data []byte) error
}

// filesystemStorage stores compressed models to local filesystem.
type filesystemStorage struct {
	outputPath string
}

func newFilesystemStorage(outputPath string) *filesystemStorage {
	return &filesystemStorage{outputPath: outputPath}
}

func (s *filesystemStorage) Store(data []byte) error {
	// Ensure directory exists
	if err := os.MkdirAll(s.outputPath, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// Write with timestamp-based filename
	filename := fmt.Sprintf("tpack_model_%s.pb", time.Now().Format("20060102_150405"))
	path := filepath.Join(s.outputPath, filename)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write model file: %w", err)
	}

	return nil
}
