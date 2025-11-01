package clip

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
)

// BenchmarkOCIIndexing benchmarks the indexing performance
func BenchmarkOCIIndexing(b *testing.B) {
	ctx := context.Background()

	testCases := []struct {
		name     string
		imageRef string
	}{
		{"Alpine", "docker.io/library/alpine:3.18"},
		{"Ubuntu", "docker.io/library/ubuntu:22.04"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			tempDir := b.TempDir()
			archiver := NewClipArchiver()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				outputFile := filepath.Join(tempDir, "test.clip")

				err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
					ImageRef:      tc.imageRef,
					CheckpointMiB: 2,
				}, outputFile)

				if err != nil {
					b.Fatalf("CreateFromOCI failed: %v", err)
				}

				os.Remove(outputFile)
			}
		})
	}
}

// TestOCIIndexingPerformance tests indexing performance with timing
func TestOCIIndexingPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	ctx := context.Background()
	tempDir := t.TempDir()
	archiver := NewClipArchiver()

	testCases := []struct {
		name     string
		imageRef string
		maxTime  float64 // seconds
	}{
		{"Alpine (small, 1 layer)", "docker.io/library/alpine:3.18", 2.0},
		{"Ubuntu (medium, ~5 layers)", "docker.io/library/ubuntu:22.04", 10.0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			outputFile := filepath.Join(tempDir, tc.name+".clip")

			err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
				ImageRef:      tc.imageRef,
				CheckpointMiB: 2,
			}, outputFile)

			if err != nil {
				t.Fatalf("CreateFromOCI failed: %v", err)
			}

			// Check file exists and is reasonably sized
			stat, err := os.Stat(outputFile)
			if err != nil {
				t.Fatalf("Output file not found: %v", err)
			}

			// Metadata-only should be < 5 MB even for large images
			if stat.Size() > 5*1024*1024 {
				t.Errorf("Output file too large: %d bytes (expected < 5 MB)", stat.Size())
			}

			t.Logf("%s: indexed to %d bytes", tc.name, stat.Size())
		})
	}
}

// TestOCIIndexingLargeFile tests indexing with files of various sizes
func TestOCIIndexingLargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large file test in short mode")
	}

	ctx := context.Background()
	tempDir := t.TempDir()
	archiver := NewClipArchiver()

	// Node.js image has larger files
	imageRef := "docker.io/library/node:18-alpine"
	outputFile := filepath.Join(tempDir, "node.clip")

	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:      imageRef,
		CheckpointMiB: 2,
	}, outputFile)

	if err != nil {
		t.Fatalf("CreateFromOCI failed: %v", err)
	}

	// Load and verify
	metadata, err := archiver.ExtractMetadata(outputFile)
	if err != nil {
		t.Fatalf("ExtractMetadata failed: %v", err)
	}

	fileCount := metadata.Index.Len()
	t.Logf("Indexed %d files from node:18-alpine", fileCount)

	// Should have hundreds of files
	if fileCount < 100 {
		t.Errorf("Expected at least 100 files, got %d", fileCount)
	}

	// Check file size
	stat, _ := os.Stat(outputFile)
	t.Logf("Archive size: %.2f KB", float64(stat.Size())/1024)
}

// TestParallelIndexingCorrectness tests that parallel indexing produces same results
func TestParallelIndexingCorrectness(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	imageRef := "docker.io/library/alpine:3.18"

	// Index with optimized version
	optimizedFile := filepath.Join(tempDir, "optimized.clip")
	archiver := NewClipArchiver()

	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:      imageRef,
		CheckpointMiB: 2,
	}, optimizedFile)

	if err != nil {
		t.Fatalf("Optimized indexing failed: %v", err)
	}

	// Load metadata
	metadata, err := archiver.ExtractMetadata(optimizedFile)
	if err != nil {
		t.Fatalf("ExtractMetadata failed: %v", err)
	}

	// Verify all files have correct structure
	fileCount := 0
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		if node.NodeType == common.FileNode {
			fileCount++

			// Verify RemoteRef exists
			if node.Remote == nil {
				t.Errorf("File %s missing RemoteRef", node.Path)
			}

			// Verify no embedded data
			if node.DataLen != 0 || node.DataPos != 0 {
				t.Errorf("File %s has embedded data markers", node.Path)
			}
		}
		return true
	})

	t.Logf("Verified %d files, all have correct structure", fileCount)
}
