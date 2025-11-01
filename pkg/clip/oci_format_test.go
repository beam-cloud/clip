package clip

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOCIArchiveIsMetadataOnly verifies that OCI mode creates a metadata-only .clip file
// with NO embedded file data
func TestOCIArchiveIsMetadataOnly(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	// Use ubuntu image (large, ~80MB uncompressed)
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	// Create OCI index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err, "CreateFromOCIImage should succeed")
	
	// Check file exists
	stat, err := os.Stat(clipFile)
	require.NoError(t, err, "clip file should exist")
	
	fileSize := stat.Size()
	t.Logf("Clip file size: %d bytes (%.2f KB)", fileSize, float64(fileSize)/1024)
	
	// CRITICAL: For ubuntu:24.04, the uncompressed size is ~80MB
	// The metadata-only clip file should be < 1MB (typically ~100-500KB)
	// If it's close to 80MB, it contains data!
	maxMetadataSize := int64(1 * 1024 * 1024) // 1MB max for metadata
	assert.Less(t, fileSize, maxMetadataSize, 
		"OCI clip file should be metadata-only (< 1MB), but got %d bytes. "+
		"This suggests file data is embedded, which is wrong for v2!", fileSize)
	
	// More specifically, for alpine which is small, metadata should be < 200KB
	assert.Less(t, fileSize, int64(200*1024),
		"Alpine metadata should be < 200KB, got %d bytes", fileSize)
	
	// Load metadata and verify structure
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err, "should load metadata")
	
	// Verify it's OCI storage type
	require.NotNil(t, metadata.StorageInfo, "storage info should exist")
	assert.Equal(t, "oci", metadata.StorageInfo.Type(), "storage type should be 'oci'")
	
	// Verify index contains nodes
	require.NotNil(t, metadata.Index, "index should exist")
	fileCount := metadata.Index.Len()
	assert.Greater(t, fileCount, 0, "index should contain files")
	t.Logf("Index contains %d files", fileCount)
	
	// Verify nodes use Remote refs (not DataPos/DataLen)
	foundRemoteRef := false
	foundEmbeddedData := false
	
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		if node.NodeType == common.FileNode {
			// Check if file node uses remote ref
			if node.Remote != nil {
				foundRemoteRef = true
				// Remote should have layer digest
				assert.NotEmpty(t, node.Remote.LayerDigest, 
					"file %s should have layer digest", node.Path)
			}
			// Check if file has embedded data markers
			if node.DataLen > 0 || node.DataPos > 0 {
				foundEmbeddedData = true
				t.Errorf("file %s has DataLen=%d or DataPos=%d, which suggests embedded data!",
					node.Path, node.DataLen, node.DataPos)
			}
		}
		return true
	})
	
	assert.True(t, foundRemoteRef, "should find at least one file with remote ref")
	assert.False(t, foundEmbeddedData, 
		"NO files should have DataLen/DataPos - this indicates embedded data!")
	
	// Verify OCI storage info has required fields
	ociInfo, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
	if !ok {
		t.Logf("StorageInfo type: %T", metadata.StorageInfo)
		// Try as interface value
		if si, ok2 := metadata.StorageInfo.(common.OCIStorageInfo); ok2 {
			ociInfoCopy := si
			ociInfo = &ociInfoCopy
			ok = true
		}
	}
	require.True(t, ok, "storage info should be OCIStorageInfo, got %T", metadata.StorageInfo)
	
	assert.NotEmpty(t, ociInfo.RegistryURL, "should have registry URL")
	assert.NotEmpty(t, ociInfo.Repository, "should have repository")
	assert.NotEmpty(t, ociInfo.Reference, "should have reference")
	assert.Greater(t, len(ociInfo.Layers), 0, "should have layer digests")
	assert.NotNil(t, ociInfo.GzipIdxByLayer, "should have gzip indexes")
	
	t.Logf("OCI Info: registry=%s, repo=%s, ref=%s, layers=%d", 
		ociInfo.RegistryURL, ociInfo.Repository, ociInfo.Reference, len(ociInfo.Layers))
	
	// Verify image metadata is embedded
	require.NotNil(t, ociInfo.ImageMetadata, "should have embedded image metadata")
	assert.NotEmpty(t, ociInfo.ImageMetadata.Architecture, "should have architecture")
	assert.NotEmpty(t, ociInfo.ImageMetadata.Os, "should have OS")
	assert.Greater(t, len(ociInfo.ImageMetadata.Layers), 0, "should have layers in metadata")
	assert.Greater(t, len(ociInfo.ImageMetadata.LayersData), 0, "should have layer data")
	
	t.Logf("Image Metadata: arch=%s, os=%s, created=%s, env_count=%d", 
		ociInfo.ImageMetadata.Architecture,
		ociInfo.ImageMetadata.Os,
		ociInfo.ImageMetadata.Created.Format("2006-01-02"),
		len(ociInfo.ImageMetadata.Env))
}

// TestOCIArchiveNoRCLIP verifies that OCI mode does NOT create RCLIP files
func TestOCIArchiveNoRCLIP(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	// Create OCI index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Verify NO .rclip file was created
	rclipFile := clipFile + ".rclip"
	_, err = os.Stat(rclipFile)
	assert.True(t, os.IsNotExist(err), 
		"RCLIP file should NOT exist for OCI mode, but found: %s", rclipFile)
	
	// Verify ONLY the .clip file exists
	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	
	clipCount := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".clip" {
			clipCount++
		}
		// Should not have any .rclip files
		assert.NotEqual(t, ".rclip", filepath.Ext(entry.Name()),
			"found unexpected .rclip file: %s", entry.Name())
	}
	
	assert.Equal(t, 1, clipCount, "should have exactly 1 .clip file")
}

// TestOCIArchiveFileContentNotEmbedded verifies specific files don't have embedded content
func TestOCIArchiveFileContentNotEmbedded(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Load metadata
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	// Check specific known files
	testFiles := []string{
		"/bin/sh",
		"/etc/alpine-release",
		"/lib/libc.musl-x86_64.so.1",
	}
	
	for _, path := range testFiles {
		node := metadata.Get(path)
		if node == nil {
			t.Logf("File %s not found (ok, may not exist in this image version)", path)
			continue
		}
		
		if node.NodeType != common.FileNode {
			continue
		}
		
		// File MUST have Remote ref
		assert.NotNil(t, node.Remote, 
			"file %s should have Remote ref (v2 OCI mode)", path)
		
		if node.Remote != nil {
			// Remote ref should have layer digest and offsets
			assert.NotEmpty(t, node.Remote.LayerDigest,
				"file %s should have layer digest", path)
			assert.GreaterOrEqual(t, node.Remote.ULength, int64(0),
				"file %s should have valid ULength", path)
			
			t.Logf("File %s: layer=%s, offset=%d, length=%d",
				path, node.Remote.LayerDigest[:12], node.Remote.UOffset, node.Remote.ULength)
		}
		
		// File MUST NOT have embedded data pointers
		assert.Equal(t, int64(0), node.DataPos,
			"file %s should NOT have DataPos (indicates embedded data)", path)
		assert.Equal(t, int64(0), node.DataLen,
			"file %s should NOT have DataLen (indicates embedded data)", path)
	}
}

// TestOCIArchiveFormatVersion verifies correct format version
func TestOCIArchiveFormatVersion(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Read header directly
	f, err := os.Open(clipFile)
	require.NoError(t, err)
	defer f.Close()
	
	headerBytes := make([]byte, common.ClipHeaderLength)
	_, err = f.Read(headerBytes)
	require.NoError(t, err)
	
	archiver := NewClipArchiver()
	header, err := archiver.DecodeHeader(headerBytes)
	require.NoError(t, err)
	
	// Verify header
	assert.Equal(t, common.ClipFileStartBytes, header.StartBytes[:],
		"should have correct start bytes")
	assert.Equal(t, common.ClipFileFormatVersion, header.ClipFileFormatVersion,
		"should have correct format version")
	assert.Equal(t, "oci", string(header.StorageInfoType[:3]),
		"storage type should be 'oci'")
	
	// Index should exist
	assert.Greater(t, header.IndexLength, int64(0), "should have index data")
	assert.Greater(t, header.StorageInfoLength, int64(0), "should have storage info")
	
	t.Logf("Header: version=%d, index_len=%d, storage_info_len=%d",
		header.ClipFileFormatVersion, header.IndexLength, header.StorageInfoLength)
}

// TestOCIMountAndReadFilesLazily tests mounting and reading files from OCI archive
func TestOCIMountAndReadFilesLazily(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping FUSE mount test in short mode")
	}
	
	// This test requires FUSE to be available
	t.Skip("Skipping FUSE-dependent test - requires fusermount and FUSE kernel module")
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	mountPoint := filepath.Join(tempDir, "mnt")
	
	// Create mount point
	err := os.MkdirAll(mountPoint, 0755)
	require.NoError(t, err)
	
	// Create OCI index
	err = CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Verify clip file is small
	stat, err := os.Stat(clipFile)
	require.NoError(t, err)
	assert.Less(t, stat.Size(), int64(200*1024),
		"clip file should be < 200KB (metadata only)")
	
	// Mount the archive
	startServer, serverError, server, err := MountArchive(MountOptions{
		ArchivePath: clipFile,
		MountPoint:  mountPoint,
	})
	require.NoError(t, err, "MountArchive should succeed")
	
	// Start FUSE server
	err = startServer()
	require.NoError(t, err, "startServer should succeed")
	defer server.Unmount()
	
	// Monitor for errors
	go func() {
		if err := <-serverError; err != nil {
			t.Logf("FUSE server error: %v", err)
		}
	}()
	
	// Wait for mount
	err = server.WaitMount()
	require.NoError(t, err, "WaitMount should succeed")
	
	// Verify mount is accessible
	entries, err := os.ReadDir(mountPoint)
	require.NoError(t, err, "should be able to read mount point")
	assert.Greater(t, len(entries), 0, "mount point should have entries")
	
	t.Logf("Mount has %d top-level entries", len(entries))
	
	// Try to read a specific file (lazy load from OCI registry)
	testFile := filepath.Join(mountPoint, "etc", "alpine-release")
	data, err := os.ReadFile(testFile)
	if err != nil {
		// File might not exist in all versions
		t.Logf("Could not read %s: %v (may not exist)", testFile, err)
	} else {
		assert.Greater(t, len(data), 0, "file should have content")
		t.Logf("Read %d bytes from %s: %s", len(data), testFile, string(data[:min(50, len(data))]))
		
		// This proves:
		// 1. Mount worked
		// 2. File metadata was in the index
		// 3. File content was lazily loaded from OCI registry
		// 4. NO embedded data in the .clip file
	}
	
	// Test symlink
	binSh := filepath.Join(mountPoint, "bin", "sh")
	target, err := os.Readlink(binSh)
	if err == nil {
		assert.NotEmpty(t, target, "symlink should have target")
		t.Logf("Symlink /bin/sh -> %s", target)
	}
	
	// Verify we can stat files (proves index is correct)
	etcDir := filepath.Join(mountPoint, "etc")
	etcStat, err := os.Stat(etcDir)
	if err == nil {
		assert.True(t, etcStat.IsDir(), "/etc should be a directory")
		t.Logf("Successfully stat'd /etc")
	}
}

// TestCompareOCIvsLegacyArchiveSize compares OCI vs legacy archive sizes
func TestCompareOCIvsLegacyArchiveSize(t *testing.T) {
	t.Skip("This is a demonstration test - creates large archives")
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/ubuntu:24.04"
	
	// Create OCI (v2) archive
	ociFile := filepath.Join(tempDir, "ubuntu-v2.clip")
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    ociFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	ociStat, err := os.Stat(ociFile)
	require.NoError(t, err)
	ociSize := ociStat.Size()
	
	fmt.Printf("OCI (v2) archive size: %.2f KB\n", float64(ociSize)/1024)
	
	// For comparison, a legacy v1 archive of ubuntu:24.04 would be ~80 MB
	// v2 should be < 1 MB
	assert.Less(t, ociSize, int64(1*1024*1024),
		"OCI archive should be < 1MB, got %.2f MB", float64(ociSize)/(1024*1024))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestOCIImageMetadataExtraction tests that image metadata is properly extracted and stored
func TestOCIImageMetadataExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	// Create OCI index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Load metadata
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	// Get OCI storage info
	ociInfo, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
	if !ok {
		if si, ok2 := metadata.StorageInfo.(common.OCIStorageInfo); ok2 {
			ociInfoCopy := si
			ociInfo = &ociInfoCopy
		} else {
			t.Fatalf("storage info should be OCIStorageInfo, got %T", metadata.StorageInfo)
		}
	}
	
	// Verify image metadata exists
	require.NotNil(t, ociInfo.ImageMetadata, "image metadata should be present")
	imgMeta := ociInfo.ImageMetadata
	
	// Verify image identification
	assert.Equal(t, imageRef, imgMeta.Name, "image name should match")
	assert.NotEmpty(t, imgMeta.Digest, "should have image digest")
	t.Logf("Image: %s (digest: %s)", imgMeta.Name, imgMeta.Digest[:20]+"...")
	
	// Verify platform information
	assert.Equal(t, "amd64", imgMeta.Architecture, "alpine should be amd64")
	assert.Equal(t, "linux", imgMeta.Os, "alpine should be linux")
	t.Logf("Platform: %s/%s", imgMeta.Os, imgMeta.Architecture)
	
	// Verify creation time
	assert.False(t, imgMeta.Created.IsZero(), "should have creation time")
	t.Logf("Created: %s", imgMeta.Created.Format(time.RFC3339))
	
	// Verify layer information
	assert.Greater(t, len(imgMeta.Layers), 0, "should have at least one layer")
	assert.Equal(t, len(imgMeta.Layers), len(imgMeta.LayersData), "layers and layer data should match")
	t.Logf("Layers: %d", len(imgMeta.Layers))
	
	// Verify layer data details
	for i, layerData := range imgMeta.LayersData {
		assert.NotEmpty(t, layerData.Digest, "layer %d should have digest", i)
		assert.NotEmpty(t, layerData.MIMEType, "layer %d should have MIME type", i)
		assert.Greater(t, layerData.Size, int64(0), "layer %d should have size", i)
		t.Logf("  Layer %d: %s (size: %d, type: %s)", 
			i, layerData.Digest[:20]+"...", layerData.Size, layerData.MIMEType)
	}
	
	// Verify runtime configuration
	// Alpine typically has minimal env vars
	t.Logf("Env vars: %d", len(imgMeta.Env))
	if len(imgMeta.Env) > 0 {
		t.Logf("  First env: %s", imgMeta.Env[0])
	}
	
	// Verify command configuration
	if len(imgMeta.Cmd) > 0 {
		t.Logf("Cmd: %v", imgMeta.Cmd)
	}
	
	// Verify labels (if any)
	t.Logf("Labels: %d", len(imgMeta.Labels))
	for key, value := range imgMeta.Labels {
		t.Logf("  %s: %s", key, value)
	}
}

// TestOCIImageMetadataCompatibility verifies metadata format matches beta9 expectations
func TestOCIImageMetadataCompatibility(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	// Create OCI index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Load metadata
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	// Get OCI storage info
	ociInfo, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
	if !ok {
		if si, ok2 := metadata.StorageInfo.(common.OCIStorageInfo); ok2 {
			ociInfoCopy := si
			ociInfo = &ociInfoCopy
		} else {
			t.Fatalf("storage info should be OCIStorageInfo, got %T", metadata.StorageInfo)
		}
	}
	
	require.NotNil(t, ociInfo.ImageMetadata, "image metadata should be present")
	imgMeta := ociInfo.ImageMetadata
	
	// Verify all beta9 required fields are present
	// From the user's ImageMetadata struct:
	assert.NotEmpty(t, imgMeta.Name, "Name should be set")
	assert.NotEmpty(t, imgMeta.Digest, "Digest should be set")
	// RepoTags is optional
	assert.False(t, imgMeta.Created.IsZero(), "Created should be set")
	// DockerVersion is optional
	// Labels is optional but should be non-nil map
	assert.NotNil(t, imgMeta.Labels, "Labels should be a non-nil map")
	assert.NotEmpty(t, imgMeta.Architecture, "Architecture should be set")
	assert.NotEmpty(t, imgMeta.Os, "Os should be set")
	assert.NotEmpty(t, imgMeta.Layers, "Layers should be set")
	assert.NotEmpty(t, imgMeta.LayersData, "LayersData should be set")
	// Env is optional but should be non-nil slice
	assert.NotNil(t, imgMeta.Env, "Env should be a non-nil slice")
	
	// Verify LayersData has required fields
	for i, layerData := range imgMeta.LayersData {
		assert.NotEmpty(t, layerData.MIMEType, "layer %d should have MIMEType", i)
		assert.NotEmpty(t, layerData.Digest, "layer %d should have Digest", i)
		assert.Greater(t, layerData.Size, int64(0), "layer %d should have Size > 0", i)
		// Annotations is optional
	}
	
	t.Logf("? Image metadata is compatible with beta9 format")
	t.Logf("  Name: %s", imgMeta.Name)
	t.Logf("  Digest: %s", imgMeta.Digest)
	t.Logf("  Architecture: %s", imgMeta.Architecture)
	t.Logf("  OS: %s", imgMeta.Os)
	t.Logf("  Layers: %d", len(imgMeta.Layers))
	t.Logf("  Created: %s", imgMeta.Created.Format(time.RFC3339))
}
