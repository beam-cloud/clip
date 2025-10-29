package clip

import (
	"context"
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
	}
	
	t.Logf("Index contains %d files across %d layers", metadata.Index.Len(), len(ociInfo.Layers))
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
