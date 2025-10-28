package clip

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOCIIndexingSkipsRuntimeDirectories verifies that /proc, /sys, /dev are NOT indexed
func TestOCIIndexingSkipsRuntimeDirectories(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	// Use ubuntu which definitely has /proc, /sys, /dev in the tar
	imageRef := "docker.io/library/ubuntu:22.04"
	clipFile := filepath.Join(tempDir, "ubuntu.clip")
	
	// Create OCI index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
		Verbose:       false,
	})
	require.NoError(t, err, "CreateFromOCIImage should succeed")
	
	// Load metadata
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err, "ExtractMetadata should succeed")
	
	// Check that runtime directories are NOT in the index
	runtimeDirs := []string{"/proc", "/sys", "/dev"}
	
	for _, dir := range runtimeDirs {
		node := metadata.Get(dir)
		assert.Nil(t, node, "Runtime directory %s should NOT be in index", dir)
	}
	
	t.Logf("✓ Verified /proc, /sys, /dev are not in index")
	
	// Verify other directories ARE present
	requiredDirs := []string{"/", "/etc", "/usr", "/var"}
	
	for _, dir := range requiredDirs {
		node := metadata.Get(dir)
		assert.NotNil(t, node, "Required directory %s should be in index", dir)
		if node != nil {
			assert.Equal(t, common.DirNode, node.NodeType, "%s should be a directory", dir)
		}
	}
	
	t.Logf("✓ Verified required directories are present")
}

// TestOCIIndexingRuntimeDirectoriesCorrectness ensures the fix doesn't break other functionality
func TestOCIIndexingRuntimeDirectoriesCorrectness(t *testing.T) {
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
	
	// Load and verify structure
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	fileCount := metadata.Index.Len()
	t.Logf("Index contains %d entries", fileCount)
	
	// Should still have plenty of files (alpine has ~527)
	assert.Greater(t, fileCount, 500, "should have > 500 files")
	
	// Verify no runtime dirs
	assert.Nil(t, metadata.Get("/proc"), "/proc should not exist")
	assert.Nil(t, metadata.Get("/sys"), "/sys should not exist")
	assert.Nil(t, metadata.Get("/dev"), "/dev should not exist")
	
	// Verify root and other dirs exist
	rootNode := metadata.Get("/")
	require.NotNil(t, rootNode, "/ should exist")
	assert.Equal(t, common.DirNode, rootNode.NodeType)
	
	etcNode := metadata.Get("/etc")
	require.NotNil(t, etcNode, "/etc should exist")
	assert.Equal(t, common.DirNode, etcNode.NodeType)
	
	// Verify files still work
	binShNode := metadata.Get("/bin/sh")
	if binShNode != nil {
		assert.Equal(t, common.SymLinkNode, binShNode.NodeType, "/bin/sh should be symlink")
		assert.NotEmpty(t, binShNode.Target, "symlink should have target")
	}
	
	t.Logf("✓ Correctness verified: runtime dirs excluded, everything else works")
}

// TestIsRuntimeDirectory tests the helper function
func TestIsRuntimeDirectory(t *testing.T) {
	archiver := NewClipArchiver()
	
	testCases := []struct {
		path     string
		expected bool
	}{
		{"/proc", true},
		{"/sys", true},
		{"/dev", true},
		{"/", false},
		{"/etc", false},
		{"/usr", false},
		{"/var", false},
		{"/home", false},
		{"/tmp", false},
		{"/proc/self", false}, // Subdirectories should not match
		{"/dev/null", false},  // Files in /dev should not match
	}
	
	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			result := archiver.isRuntimeDirectory(tc.path)
			assert.Equal(t, tc.expected, result, "isRuntimeDirectory(%s) should be %v", tc.path, tc.expected)
		})
	}
}
