package clip

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOCIIndexing tests the OCI image indexing workflow
func TestOCIIndexing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// Use a small public image for testing
	imageRef := "docker.io/library/alpine:3.18"

	// Create temporary output file
	tempDir := t.TempDir()
	outputFile := filepath.Join(tempDir, "alpine.clip")

	// Test indexing
	archiver := NewClipArchiver()
	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:      imageRef,
		CheckpointMiB: 2,
	}, outputFile)

	require.NoError(t, err, "Failed to index OCI image")

	// Verify output file exists
	info, err := os.Stat(outputFile)
	require.NoError(t, err, "Output file should exist")
	assert.Greater(t, info.Size(), int64(0), "Output file should not be empty")

	t.Logf("Created index file: %s (size: %d bytes)", outputFile, info.Size())

	// Load and verify metadata
	metadata, err := archiver.ExtractMetadata(outputFile)
	require.NoError(t, err, "Should be able to extract metadata")

	assert.NotNil(t, metadata.Index, "Index should not be nil")
	assert.Greater(t, metadata.Index.Len(), 0, "Index should contain nodes")

	// Verify storage info
	require.NotNil(t, metadata.StorageInfo, "Storage info should not be nil")

	ociInfo, ok := metadata.StorageInfo.(common.OCIStorageInfo)
	if !ok {
		ociInfoPtr, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
		require.True(t, ok, "Storage info should be OCIStorageInfo")
		ociInfo = *ociInfoPtr
	}

	assert.Equal(t, "oci", ociInfo.Type(), "Storage type should be oci")
	assert.Greater(t, len(ociInfo.Layers), 0, "Should have at least one layer")
	assert.NotNil(t, ociInfo.GzipIdxByLayer, "Should have gzip indexes")

	// Verify gzip indexes exist for each layer
	for _, layerDigest := range ociInfo.Layers {
		idx, ok := ociInfo.GzipIdxByLayer[layerDigest]
		assert.True(t, ok, "Should have gzip index for layer %s", layerDigest)
		assert.Greater(t, len(idx.Checkpoints), 0, "Should have checkpoints for layer %s", layerDigest)
		t.Logf("Layer %s has %d checkpoints", layerDigest, len(idx.Checkpoints))
	}

	// Verify decompressed hashes exist for each layer (used for content-addressed caching)
	assert.NotNil(t, ociInfo.DecompressedHashByLayer, "Should have decompressed hash map")
	for _, layerDigest := range ociInfo.Layers {
		hash, ok := ociInfo.DecompressedHashByLayer[layerDigest]
		assert.True(t, ok, "Should have decompressed hash for layer %s", layerDigest)
		assert.NotEmpty(t, hash, "Decompressed hash should not be empty")
		assert.Len(t, hash, 64, "Decompressed hash should be SHA256 (64 hex chars)")
		t.Logf("Layer %s decompressed_hash=%s", layerDigest, hash)
	}

	t.Logf("Index contains %d files across %d layers", metadata.Index.Len(), len(ociInfo.Layers))
}

// BenchmarkCheckpointIntervals tests different checkpoint intervals
func BenchmarkOCICheckpointIntervals(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	intervals := []int64{1, 2, 4, 8}

	for _, interval := range intervals {
		b.Run(fmt.Sprintf("%dMiB", interval), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ctx := context.Background()
				tempDir := b.TempDir()

				err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
					ImageRef:      "docker.io/library/alpine:3.18",
					OutputPath:    filepath.Join(tempDir, "test.clip"),
					CheckpointMiB: interval,
				})

				if err != nil {
					b.Fatalf("Failed to index: %v", err)
				}
			}
		})
	}
}

// TestCheckpointPerformance measures performance across different intervals
func TestOCICheckpointPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	intervals := []int64{1, 2, 4, 8}
	ctx := context.Background()

	t.Log("Testing checkpoint intervals on Alpine image:")
	for _, interval := range intervals {
		tempDir := t.TempDir()

		start := time.Now()
		err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
			ImageRef:      "docker.io/library/alpine:3.18",
			OutputPath:    tempDir + "/test.clip",
			CheckpointMiB: interval,
		})
		duration := time.Since(start)

		if err != nil {
			t.Fatalf("Failed with interval %d MiB: %v", interval, err)
		}

		t.Logf("Interval %2d MiB: %v", interval, duration)
	}
}

// TestOCIMountAndRead tests mounting an OCI archive and reading files
func TestOCIMountAndRead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test requires FUSE to be available
	t.Skip("Skipping FUSE-dependent test - requires fusermount and FUSE kernel module")

	ctx := context.Background()

	// Use alpine for testing (small and has known files)
	imageRef := "docker.io/library/alpine:3.18"

	tempDir := t.TempDir()
	clipFile := filepath.Join(tempDir, "alpine.clip")
	mountPoint := filepath.Join(tempDir, "mnt")

	// Step 1: Create OCI index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err, "Failed to create OCI index")

	// Step 2: Mount the archive
	err = os.MkdirAll(mountPoint, 0755)
	require.NoError(t, err, "Failed to create mount point")

	startServer, serverError, server, err := MountArchive(MountOptions{
		ArchivePath:           clipFile,
		MountPoint:            mountPoint,
		ContentCacheAvailable: false,
	})
	require.NoError(t, err, "Failed to mount archive")
	defer server.Unmount()

	// Start the server
	err = startServer()
	require.NoError(t, err, "Failed to start server")

	// Wait for mount to be ready or error
	select {
	case err := <-serverError:
		if err != nil {
			t.Fatalf("Server error: %v", err)
		}
	default:
		// Give it a moment to mount
		time.Sleep(500 * time.Millisecond)
	}

	// Step 3: Read and verify files
	t.Run("ReadRootDirectory", func(t *testing.T) {
		entries, err := os.ReadDir(mountPoint)
		require.NoError(t, err, "Should be able to read root directory")
		assert.Greater(t, len(entries), 0, "Root should contain entries")

		t.Logf("Root directory contains %d entries", len(entries))
		for _, entry := range entries {
			t.Logf("  - %s (dir=%v)", entry.Name(), entry.IsDir())
		}
	})

	t.Run("ReadEtcDirectory", func(t *testing.T) {
		etcPath := filepath.Join(mountPoint, "etc")
		_, err := os.Stat(etcPath)
		require.NoError(t, err, "/etc should exist")

		entries, err := os.ReadDir(etcPath)
		require.NoError(t, err, "Should be able to read /etc")
		assert.Greater(t, len(entries), 0, "/etc should contain files")
	})

	t.Run("ReadOSReleaseFile", func(t *testing.T) {
		osReleasePath := filepath.Join(mountPoint, "etc", "os-release")
		data, err := os.ReadFile(osReleasePath)
		require.NoError(t, err, "Should be able to read /etc/os-release")
		assert.Greater(t, len(data), 0, "File should have content")
		assert.Contains(t, string(data), "Alpine", "Should contain Alpine identifier")

		t.Logf("Read %d bytes from /etc/os-release", len(data))
	})

	t.Run("ReadBinDirectory", func(t *testing.T) {
		binPath := filepath.Join(mountPoint, "bin")
		entries, err := os.ReadDir(binPath)
		require.NoError(t, err, "Should be able to read /bin")
		assert.Greater(t, len(entries), 0, "/bin should contain executables")

		// Check for common executables
		hasLs := false
		for _, entry := range entries {
			if entry.Name() == "ls" || entry.Name() == "busybox" {
				hasLs = true
				break
			}
		}
		assert.True(t, hasLs, "/bin should contain ls or busybox")
	})
}

// TestOCIWithContentCache tests OCI mounting with content cache enabled
func TestOCIWithContentCache(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test requires FUSE to be available
	t.Skip("Skipping FUSE-dependent test - requires fusermount and FUSE kernel module")

	ctx := context.Background()
	imageRef := "docker.io/library/alpine:3.18"

	tempDir := t.TempDir()
	clipFile := filepath.Join(tempDir, "alpine.clip")
	mountPoint := filepath.Join(tempDir, "mnt")

	// Create index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)

	// Create mock content cache
	mockCache := newMockContentCache()

	// Mount with cache
	err = os.MkdirAll(mountPoint, 0755)
	require.NoError(t, err)

	startServer, _, server, err := MountArchive(MountOptions{
		ArchivePath:           clipFile,
		MountPoint:            mountPoint,
		ContentCache:          mockCache,
		ContentCacheAvailable: true,
	})
	require.NoError(t, err)
	defer server.Unmount()

	err = startServer()
	require.NoError(t, err)

	// Wait for mount
	time.Sleep(500 * time.Millisecond)

	// Read a file (should populate cache)
	osReleasePath := filepath.Join(mountPoint, "etc", "os-release")
	data1, err := os.ReadFile(osReleasePath)
	require.NoError(t, err)

	// Read again (should hit cache)
	data2, err := os.ReadFile(osReleasePath)
	require.NoError(t, err)

	assert.Equal(t, data1, data2, "File content should be consistent")
	t.Logf("Read file successfully with cache enabled")
}

// TestProgrammaticAPI tests the programmatic API
func TestProgrammaticAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	tempDir := t.TempDir()

	t.Run("CreateFromOCIImage", func(t *testing.T) {
		outputPath := filepath.Join(tempDir, "test-alpine.clip")

		err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
			ImageRef:      "docker.io/library/alpine:3.18",
			OutputPath:    outputPath,
			CheckpointMiB: 2,
		})

		require.NoError(t, err, "CreateFromOCIImage should succeed")

		// Verify file exists
		_, err = os.Stat(outputPath)
		require.NoError(t, err, "Output file should exist")
	})
}

// Use the mock from fsnode_test.go

// TestOCIStorageReadFile tests the OCI storage ReadFile method directly
func TestOCIStorageReadFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	tempDir := t.TempDir()
	clipFile := filepath.Join(tempDir, "alpine.clip")

	// Create index
	archiver := NewClipArchiver()
	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:      "docker.io/library/alpine:3.18",
		CheckpointMiB: 2,
	}, clipFile)
	require.NoError(t, err)

	// Load metadata
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)

	// Create OCI storage
	ociStorage, err := storage.NewOCIClipStorage(storage.OCIClipStorageOpts{
		Metadata: metadata,
	})
	require.NoError(t, err)
	defer ociStorage.Cleanup()

	// Find a file node with RemoteRef
	var testNode *common.ClipNode
	metadata.Index.Ascend(metadata.Index.Min(), func(item interface{}) bool {
		node := item.(*common.ClipNode)
		if node.NodeType == common.FileNode && node.Remote != nil && node.Remote.ULength > 0 {
			testNode = node
			return false // stop iteration
		}
		return true
	})

	require.NotNil(t, testNode, "Should find at least one file node with RemoteRef")
	t.Logf("Testing with file: %s (size: %d)", testNode.Path, testNode.Remote.ULength)

	// Read the file
	dest := make([]byte, testNode.Remote.ULength)
	nRead, err := ociStorage.ReadFile(testNode, dest, 0)
	require.NoError(t, err, "Should be able to read file")
	assert.Equal(t, int(testNode.Remote.ULength), nRead, "Should read expected number of bytes")

	// Test partial read
	if testNode.Remote.ULength > 10 {
		partial := make([]byte, 10)
		nRead, err = ociStorage.ReadFile(testNode, partial, 0)
		require.NoError(t, err, "Should be able to read partial")
		assert.Equal(t, 10, nRead, "Should read 10 bytes")
	}
}

// TestLayerCaching verifies that layers are properly cached after first read
func TestLayerCaching(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	tempDir := t.TempDir()
	clipFile := filepath.Join(tempDir, "alpine.clip")

	// Create index
	archiver := NewClipArchiver()
	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:      "docker.io/library/alpine:3.18",
		CheckpointMiB: 2,
	}, clipFile)
	require.NoError(t, err)

	// Load metadata
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)

	// Verify decompressed hashes are present
	ociInfo, ok := metadata.StorageInfo.(common.OCIStorageInfo)
	if !ok {
		ociInfoPtr, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
		require.True(t, ok, "Storage info should be OCIStorageInfo")
		ociInfo = *ociInfoPtr
	}

	require.NotNil(t, ociInfo.DecompressedHashByLayer, "Should have decompressed hashes")
	require.Greater(t, len(ociInfo.DecompressedHashByLayer), 0, "Should have at least one layer hash")

	// Create OCI storage with custom cache dir
	cacheDir := filepath.Join(tempDir, "cache")
	ociStorage, err := storage.NewOCIClipStorage(storage.OCIClipStorageOpts{
		Metadata:     metadata,
		DiskCacheDir: cacheDir,
	})
	require.NoError(t, err)
	defer ociStorage.Cleanup()

	// Find a file to read
	var testNode *common.ClipNode
	metadata.Index.Ascend(metadata.Index.Min(), func(item interface{}) bool {
		node := item.(*common.ClipNode)
		if node.NodeType == common.FileNode && node.Remote != nil && node.Remote.ULength > 100 {
			testNode = node
			return false
		}
		return true
	})

	require.NotNil(t, testNode, "Should find a file to test")
	layerDigest := testNode.Remote.LayerDigest
	decompressedHash, ok := ociInfo.DecompressedHashByLayer[layerDigest]
	require.True(t, ok, "Should have decompressed hash for layer")

	// Verify cache doesn't exist yet
	cachePath := filepath.Join(cacheDir, decompressedHash)
	_, err = os.Stat(cachePath)
	assert.True(t, os.IsNotExist(err), "Cache file should not exist before first read")

	// First read - should decompress and cache
	dest := make([]byte, testNode.Remote.ULength)
	_, err = ociStorage.ReadFile(testNode, dest, 0)
	require.NoError(t, err, "First read should succeed")

	// Verify cache now exists
	info, err := os.Stat(cachePath)
	require.NoError(t, err, "Cache file should exist after first read")
	assert.Greater(t, info.Size(), int64(0), "Cache file should not be empty")
	t.Logf("Layer cached at: %s (size: %d bytes)", cachePath, info.Size())

	// Second read - should use cache
	dest2 := make([]byte, testNode.Remote.ULength)
	_, err = ociStorage.ReadFile(testNode, dest2, 0)
	require.NoError(t, err, "Second read should succeed")

	// Verify data is identical
	assert.Equal(t, dest, dest2, "Data from cache should match original")
}
