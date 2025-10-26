package clip

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
)

// CreateIndexOnlyArchiveFromOCILayout creates a metadata-only .clip file from a local OCI layout directory.
// This is the integration function for beta9's build system.
//
// Parameters:
//   - ctx: Context for the operation
//   - ociLayoutPath: Path to the OCI layout directory (e.g., "/tmp/ubuntu")
//   - tag: Image tag to index (e.g., "latest")
//   - outputPath: Path for the output .clip file
//
// Returns:
//   - error: nil on success, error on failure
func CreateIndexOnlyArchiveFromOCILayout(ctx context.Context, ociLayoutPath, tag, outputPath string) error {
	log.Info().
		Str("layout_path", ociLayoutPath).
		Str("tag", tag).
		Str("output", outputPath).
		Msg("creating index-only archive from OCI layout")

	// Validate inputs
	if ociLayoutPath == "" {
		return fmt.Errorf("ociLayoutPath cannot be empty")
	}
	if tag == "" {
		tag = "latest" // default tag
	}
	if outputPath == "" {
		return fmt.Errorf("outputPath cannot be empty")
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputPath)
	if err := ensureDir(outputDir); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Create archiver and index the OCI layout
	archiver := NewClipArchiver()
	
	err := archiver.CreateFromOCILayout(ctx, ociLayoutPath, tag, outputPath)
	if err != nil {
		return fmt.Errorf("failed to create index from OCI layout: %w", err)
	}

	log.Info().
		Str("output", outputPath).
		Msg("successfully created index-only archive from OCI layout")
	
	return nil
}

// ensureDir creates a directory if it doesn't exist
func ensureDir(dir string) error {
	if dir == "" {
		return nil
	}
	
	// Check if directory already exists
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("path exists but is not a directory: %s", dir)
		}
		return nil // Directory already exists
	}
	
	// Create directory
	return os.MkdirAll(dir, 0755)
}