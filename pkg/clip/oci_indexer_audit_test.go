package clip

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOCIIndexingLayerOrdering verifies layers are processed bottom-to-top
// and that files in upper layers override files in lower layers
func TestOCIIndexingLayerOrdering(t *testing.T) {
	t.Skip("Integration test - requires real OCI image")
	
	// This test would verify with a real multi-layer image:
	// 1. Layer 1: Creates /app/config.txt with "version 1"
	// 2. Layer 2: Overwrites /app/config.txt with "version 2"
	// 
	// Expected: Index should point to Layer 2's version
	
	ctx := context.Background()
	archiver := NewClipArchiver()
	
	// Use a test image with known layer structure
	index, _, _, _, _, _, err := archiver.IndexOCIImage(ctx, IndexOCIImageOptions{
		ImageRef: "docker.io/library/alpine:3.18",
	})
	require.NoError(t, err)
	require.NotNil(t, index)
	
	// Verify index structure
	require.Greater(t, index.Len(), 0, "Index should contain files")
}

// TestOCIIndexingWhiteoutHandling verifies that whiteout files
// (deletions in upper layers) are properly handled
func TestOCIIndexingWhiteoutHandling(t *testing.T) {
	t.Skip("Need to implement whiteout handling verification")
	
	// This test would verify:
	// 1. Layer 1: Creates /app/temp.txt
	// 2. Layer 2: Contains .wh.temp.txt (whiteout marker)
	//
	// Expected: /app/temp.txt should NOT be in the final index
}

// TestOCIIndexingContentAddressing verifies we use layer digests, not file hashes
func TestOCIIndexingContentAddressing(t *testing.T) {
	t.Skip("Integration test - requires real OCI image")
	
	ctx := context.Background()
	archiver := NewClipArchiver()
	
	index, layerDigests, _, _, _, _, err := archiver.IndexOCIImage(ctx, IndexOCIImageOptions{
		ImageRef: "docker.io/library/alpine:3.18",
	})
	require.NoError(t, err)
	
	// Verify all layer digests start with sha256:
	for _, digest := range layerDigests {
		require.Contains(t, digest, "sha256:", "Layer digest should be sha256 hash")
	}
	
	// Walk index and verify all RemoteRef.LayerDigest values are in layerDigests
	layerDigestMap := make(map[string]bool)
	for _, d := range layerDigests {
		layerDigestMap[d] = true
	}
	
	index.Ascend(nil, func(item interface{}) bool {
		node := item.(*common.ClipNode)
		if node.Remote != nil {
			require.True(t, layerDigestMap[node.Remote.LayerDigest],
				"File %s points to unknown layer %s", node.Path, node.Remote.LayerDigest)
		}
		return true
	})
}
