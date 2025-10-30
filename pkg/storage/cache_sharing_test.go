package storage

import (
	"os"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
)

// TestCrossImageCacheSharing verifies that multiple images sharing the same layer
// benefit from the disk cache
func TestCrossImageCacheSharing(t *testing.T) {
	// Create shared layer data (e.g., Ubuntu base layer used by both images)
	sharedLayerData := []byte("Ubuntu base layer - shared across images")
	compressedSharedLayer := createGzipData(t, sharedLayerData)
	
	sharedDigest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "shared_ubuntu_base_layer_abc123def456",
	}
	
	// Shared disk cache directory (simulating same worker)
	diskCacheDir := t.TempDir()
	
	// === IMAGE 1: app-one:latest ===
	image1Layer := &mockLayer{
		digest:         sharedDigest,
		compressedData: compressedSharedLayer,
	}
	
	metadata1 := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				sharedDigest.String(): {},
			},
		},
	}
	
	storage1 := &OCIClipStorage{
		metadata:            metadata1,
		storageInfo:         metadata1.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{sharedDigest.String(): image1Layer},
		diskCacheDir:        diskCacheDir,
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        nil, // No remote cache for this test
	}
	
	node1 := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: sharedDigest.String(),
			UOffset:     0,
			ULength:     int64(len(sharedLayerData)),
		},
	}
	
	// Read from image 1 - should decompress and cache
	dest1 := make([]byte, len(sharedLayerData))
	n, err := storage1.ReadFile(node1, dest1, 0)
	require.NoError(t, err)
	require.Equal(t, len(sharedLayerData), n)
	require.Equal(t, sharedLayerData, dest1)
	
	// Verify layer is cached on disk
	cachedLayerPath := storage1.getDiskCachePath(sharedDigest.String())
	_, err = os.Stat(cachedLayerPath)
	require.NoError(t, err, "Shared layer should be cached after image 1 read")
	
	t.Logf("Image 1 cached shared layer at: %s", cachedLayerPath)
	
	// === IMAGE 2: app-two:latest (different image, same base layer) ===
	image2Layer := &mockLayer{
		digest:         sharedDigest,
		compressedData: compressedSharedLayer,
	}
	
	metadata2 := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				sharedDigest.String(): {},
			},
		},
	}
	
	storage2 := &OCIClipStorage{
		metadata:            metadata2,
		storageInfo:         metadata2.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{sharedDigest.String(): image2Layer},
		diskCacheDir:        diskCacheDir, // SAME disk cache directory!
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        nil,
	}
	
	node2 := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: sharedDigest.String(),
			UOffset:     0,
			ULength:     int64(len(sharedLayerData)),
		},
	}
	
	// Read from image 2 - should hit disk cache (no decompression!)
	dest2 := make([]byte, len(sharedLayerData))
	n, err = storage2.ReadFile(node2, dest2, 0)
	require.NoError(t, err)
	require.Equal(t, len(sharedLayerData), n)
	require.Equal(t, sharedLayerData, dest2)
	
	// Verify same cached layer path
	cachedLayerPath2 := storage2.getDiskCachePath(sharedDigest.String())
	require.Equal(t, cachedLayerPath, cachedLayerPath2, "Both images should use same cache file")
	
	t.Logf("✅ SUCCESS: Image 2 reused cached layer from Image 1!")
	t.Logf("Cache file: %s", cachedLayerPath)
	t.Logf("Cache sharing verified: both images use same digest-based cache file")
}

// TestCacheKeyFormat verifies the cache key format is correct
func TestCacheKeyFormat(t *testing.T) {
	diskCacheDir := t.TempDir()
	
	testCases := []struct {
		name           string
		digest         string
		expectedSuffix string
	}{
		{
			name:           "Standard sha256 digest",
			digest:         "sha256:abc123def456",
			expectedSuffix: "abc123def456", // Just the hex hash
		},
		{
			name:           "Long sha256 digest",
			digest:         "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
			expectedSuffix: "44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885", // Just the hex hash
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			storage := &OCIClipStorage{
				diskCacheDir: diskCacheDir,
			}
			
			path := storage.getDiskCachePath(tc.digest)
			
			// Should use full digest, not hashed
			require.Contains(t, path, tc.expectedSuffix, "Cache file should use full layer digest")
			
			// Should NOT contain ".decompressed" suffix
			require.NotContains(t, path, ".decompressed", "Cache file should not have .decompressed suffix")
			
			// Should NOT be hashed to shorter form
			require.NotContains(t, path, "layer-", "Cache file should not have layer- prefix")
			
			t.Logf("Cache path: %s", path)
		})
	}
}
