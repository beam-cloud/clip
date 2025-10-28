package clip

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOCIDirectoryStructureIntegrity verifies that ALL parent directories are created properly
// This is CRITICAL for runc compatibility - "wandered into deleted directory" errors occur
// when parent directories don't exist or have incorrect metadata
func TestOCIDirectoryStructureIntegrity(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	// Use Ubuntu which has deep directory structures like /usr/bin, /usr/local/bin, etc.
	imageRef := "docker.io/library/ubuntu:22.04"
	clipFile := filepath.Join(tempDir, "ubuntu.clip")
	
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
	
	// Verify root exists
	root := metadata.Get("/")
	require.NotNil(t, root, "Root directory / must exist")
	assert.Equal(t, common.DirNode, root.NodeType, "/ should be a directory")
	
	// Verify critical directory paths exist
	criticalDirs := []string{
		"/usr",
		"/usr/bin",
		"/usr/local",
		"/usr/local/bin",
		"/etc",
		"/var",
		"/var/log",
	}
	
	for _, dirPath := range criticalDirs {
		dir := metadata.Get(dirPath)
		assert.NotNil(t, dir, "Directory %s must exist", dirPath)
		if dir != nil {
			assert.Equal(t, common.DirNode, dir.NodeType, "%s must be a directory", dirPath)
			assert.NotZero(t, dir.Attr.Ino, "%s must have valid inode", dirPath)
			assert.NotZero(t, dir.Attr.Mode, "%s must have valid mode", dirPath)
			t.Logf("✓ %s exists: ino=%d mode=0%o", dirPath, dir.Attr.Ino, dir.Attr.Mode)
		}
	}
	
	// Verify that for every file, all its parent directories exist
	missingParents := 0
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		
		// Check each parent directory
		parts := filepath.Dir(node.Path)
		for parts != "/" && parts != "." {
			parent := metadata.Get(parts)
			if parent == nil {
				t.Errorf("File %s has missing parent directory: %s", node.Path, parts)
				missingParents++
				break
			}
			if parent.NodeType != common.DirNode {
				t.Errorf("Parent %s of file %s is not a directory (type=%v)", parts, node.Path, parent.NodeType)
				missingParents++
				break
			}
			parts = filepath.Dir(parts)
		}
		
		return true // Continue iteration
	})
	
	assert.Equal(t, 0, missingParents, "All files must have complete parent directory chains")
	t.Logf("✓ Verified all %d nodes have complete parent directory chains", metadata.Index.Len())
}

// TestOCIDirectoryMetadata verifies that directories have proper metadata
func TestOCIDirectoryMetadata(t *testing.T) {
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
	
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	// Check that directories have:
	// 1. Valid inode (not 0, not 1)
	// 2. Valid mode (S_IFDIR bit set)
	// 3. Non-zero permissions
	
	dirCount := 0
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		
		if node.NodeType == common.DirNode {
			dirCount++
			
			// Verify inode (0 is reserved, but 1 is valid for root)
			if node.Path == "/" {
				// Root can be inode 1
				assert.NotZero(t, node.Attr.Ino,
					"Directory %s must have valid inode (got %d)", node.Path, node.Attr.Ino)
			} else {
				// Other dirs should be > 1
				assert.Greater(t, node.Attr.Ino, uint64(1), 
					"Directory %s must have valid inode (got %d)", node.Path, node.Attr.Ino)
			}
			
			// Verify mode has S_IFDIR bit (0040000)
			assert.NotZero(t, node.Attr.Mode & 0040000, 
				"Directory %s must have S_IFDIR bit set (mode=0%o)", node.Path, node.Attr.Mode)
			
			// Verify permissions (at least one bit set)
			assert.NotZero(t, node.Attr.Mode & 0777, 
				"Directory %s must have permissions (mode=0%o)", node.Path, node.Attr.Mode)
		}
		
		return true
	})
	
	t.Logf("✓ Verified %d directories have proper metadata", dirCount)
	assert.Greater(t, dirCount, 0, "Should have found some directories")
}

// TestOCISymlinkParentDirs verifies that symlinks have their parent directories created
func TestOCISymlinkParentDirs(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	// Alpine has symlinks like /bin -> /usr/bin
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	// Find all symlinks and verify their parent directories exist
	symlinkCount := 0
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		
		if node.NodeType == common.SymLinkNode {
			symlinkCount++
			
			// Verify parent directory exists
			parentPath := filepath.Dir(node.Path)
			if parentPath != "/" && parentPath != "." {
				parent := metadata.Get(parentPath)
				assert.NotNil(t, parent, "Symlink %s must have parent directory %s", node.Path, parentPath)
				if parent != nil {
					assert.Equal(t, common.DirNode, parent.NodeType, 
						"Parent %s of symlink %s must be a directory", parentPath, node.Path)
				}
			}
		}
		
		return true
	})
	
	t.Logf("✓ Verified %d symlinks have parent directories", symlinkCount)
	assert.Greater(t, symlinkCount, 0, "Should have found some symlinks")
}

// TestOCIDeepDirectoryStructure verifies handling of deeply nested directories
func TestOCIDeepDirectoryStructure(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/ubuntu:22.04"
	clipFile := filepath.Join(tempDir, "ubuntu.clip")
	
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	archiver := NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	require.NoError(t, err)
	
	// Find the deepest path
	maxDepth := 0
	deepestPath := ""
	
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		depth := len(filepath.SplitList(node.Path))
		if depth > maxDepth {
			maxDepth = depth
			deepestPath = node.Path
		}
		return true
	})
	
	t.Logf("Deepest path: %s (depth=%d)", deepestPath, maxDepth)
	
	// Verify the deepest path has all its parents
	if deepestPath != "" {
		parts := filepath.Dir(deepestPath)
		for parts != "/" && parts != "." {
			parent := metadata.Get(parts)
			require.NotNil(t, parent, "Deep path %s missing parent %s", deepestPath, parts)
			require.Equal(t, common.DirNode, parent.NodeType, "%s should be directory", parts)
			parts = filepath.Dir(parts)
		}
	}
	
	t.Logf("✓ Verified complete directory chain for deepest path")
}
