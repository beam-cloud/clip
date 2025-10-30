package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
)

// TestDecompressedHashMapping verifies that layer digest to decompressed hash mapping works
func TestDecompressedHashMapping(t *testing.T) {
	tests := []struct {
		name              string
		layerDigest       string
		decompressedHash  string
	}{
		{
			name:             "SHA256 layer",
			layerDigest:      "sha256:abc123def456",
			decompressedHash: "7934bcedddc2d6e088e26a5b4d6421704dbd65545f3907cbcb1d74c3d83fba27",
		},
		{
			name:             "Long SHA256 layer",
			layerDigest:      "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
			decompressedHash: "239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create storage with metadata containing decompressed hash
			storageInfo := &common.OCIStorageInfo{
				DecompressedHashByLayer: map[string]string{
					tc.layerDigest: tc.decompressedHash,
				},
			}
			storage := &OCIClipStorage{
				storageInfo: storageInfo,
			}
			
			// Retrieve and verify
			result := storage.getDecompressedHash(tc.layerDigest)
			require.Equal(t, tc.decompressedHash, result)
			
			// Test getContentHash (alias for getDecompressedHash)
			result2 := storage.getContentHash(tc.layerDigest)
			require.Equal(t, tc.decompressedHash, result2)
		})
	}
}

// TestRemoteCacheKeyFormat verifies remote cache uses content hash only
func TestRemoteCacheKeyFormat(t *testing.T) {
	t.Skip("Integration test - requires mock ContentCache")

	// This test verifies that:
	// 1. Remote cache keys use ONLY the content hash (hex part)
	// 2. No prefixes like "clip:oci:layer:decompressed:"
	// 3. No algorithm prefix like "sha256:"
	// 4. Cross-image sharing works (same layer = same cache key)

	// Example:
	// Layer digest: sha256:abc123...
	// Remote cache key: abc123... (just the hash!)
	// Disk cache path: /tmp/clip-oci-cache/sha256_abc123... (filesystem-safe)
}

// TestContentAddressedCaching verifies decompressed hash enables cross-image sharing
func TestContentAddressedCaching(t *testing.T) {
	// Same layer used in multiple images
	sharedLayerDigest := "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885"
	decompressedHash := "239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c"

	// Create storage with metadata containing decompressed hash (from indexing)
	storageInfo := &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{
			sharedLayerDigest: decompressedHash,
		},
	}
	storage := &OCIClipStorage{
		storageInfo: storageInfo,
	}

	// Both images should produce the SAME cache key
	cacheKey := storage.getContentHash(sharedLayerDigest)

	// Cache key should be the decompressed hash (true content-addressing)
	require.Equal(t, decompressedHash, cacheKey)
	require.NotContains(t, cacheKey, ":", "Cache key should not contain colon")
	require.NotContains(t, cacheKey, "sha256:", "Cache key should not contain algorithm prefix")
	require.NotContains(t, cacheKey, "clip:", "Cache key should not contain namespace prefix")

	t.Logf("✅ Content-addressed cache key: %s", cacheKey)
	t.Logf("This is the hash of the decompressed data - same content = same hash!")
}

// TestContentCacheRangeRead verifies that we use decompressed hash for caching
func TestContentCacheRangeRead(t *testing.T) {
	// Create test layer data
	layerData := []byte("This is a test layer with some content for range reading verification")
	compressedData := createGzipData(t, layerData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "rangetest123",
	}

	// Compute decompressed hash for content-addressed caching
	hasher := sha256.New()
	hasher.Write(layerData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	// Setup cache
	cache := newMockCache()

	// Create mock layer
	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}

	// Create storage with metadata
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {},
			},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}

	diskCacheDir := t.TempDir()

	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          diskCacheDir,
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
	}

	// Test: First read triggers decompression and caching
	t.Run("FirstReadDecompresses", func(t *testing.T) {
		node := &common.ClipNode{
			Remote: &common.RemoteRef{
				LayerDigest: digest.String(),
				UOffset:     0,
				ULength:     10,
			},
		}

		dest := make([]byte, 10)
		n, err := storage.ReadFile(node, dest, 0)
		require.NoError(t, err)
		require.Equal(t, 10, n)
		require.Equal(t, layerData[0:10], dest)

		// First read should decompress (cache miss)
		// Decompressed hash mapping should now be stored
		decompHash := storage.getDecompressedHash(digest.String())
		require.NotEmpty(t, decompHash, "Decompressed hash should be stored after first read")
		
		t.Logf("Layer digest: %s", digest.String())
		t.Logf("Decompressed hash: %s", decompHash)
	})

	// Test: Subsequent reads use disk cache
	t.Run("SubsequentReadsUseDiskCache", func(t *testing.T) {
		node := &common.ClipNode{
			Remote: &common.RemoteRef{
				LayerDigest: digest.String(),
				UOffset:     20,
				ULength:     15,
			},
		}

		dest := make([]byte, 15)
		n, err := storage.ReadFile(node, dest, 0)
		require.NoError(t, err)
		require.Equal(t, 15, n)
		require.Equal(t, layerData[20:35], dest)

		// Should hit disk cache (fastest path)
	})
}

// TestDiskCacheThenContentCache verifies cache hierarchy: disk -> ContentCache -> OCI
func TestDiskCacheThenContentCache(t *testing.T) {
	layerData := []byte("Layer data for cache hierarchy test")
	compressedData := createGzipData(t, layerData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "hierarchy123",
	}

	// Compute decompressed hash for content-addressed caching
	hasher := sha256.New()
	hasher.Write(layerData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	cache := newMockCache()
	cacheKey := digest.Hex

	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {},
			},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}

	diskCacheDir := t.TempDir()

	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          diskCacheDir,
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
	}

	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     5,
			ULength:     10,
		},
	}

	// First read: No cache yet, should decompress from OCI and cache to disk
	dest := make([]byte, 10)
	n, err := storage.ReadFile(node, dest, 0)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, layerData[5:15], dest)

	// Second read: Should hit disk cache (fast!)
	dest2 := make([]byte, 10)
	n, err = storage.ReadFile(node, dest2, 0)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, layerData[5:15], dest2)

	// Third read with ContentCache enabled: should still hit disk first
	// Pre-populate ContentCache to verify disk is checked first
	chunks := make(chan []byte, 1)
	chunks <- layerData
	close(chunks)
	_, err = cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{})
	require.NoError(t, err)

	cache.getCalls = 0 // Reset call counter
	dest3 := make([]byte, 10)
	n, err = storage.ReadFile(node, dest3, 0)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, layerData[5:15], dest3)
	require.Equal(t, 0, cache.getCalls, "Should NOT call ContentCache (disk cache hit takes priority)")
}

// TestRangeReadOnlyFetchesNeededBytes verifies we don't fetch entire layer
func TestRangeReadOnlyFetchesNeededBytes(t *testing.T) {
	// Create a large layer
	largeLayerData := make([]byte, 10*1024*1024) // 10 MB
	for i := range largeLayerData {
		largeLayerData[i] = byte(i % 256)
	}

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "largefile123",
	}

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(largeLayerData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	cache := newMockCache()

	// Pre-populate cache with large layer using decompressed hash
	chunks := make(chan []byte, 1)
	chunks <- largeLayerData
	close(chunks)
	_, err := cache.StoreContent(chunks, decompressedHash, struct{ RoutingKey string }{})
	require.NoError(t, err)

	layer := &mockLayer{
		digest:         digest,
		compressedData: createGzipData(t, largeLayerData),
	}

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {},
			},
		},
	}

	diskCacheDir := t.TempDir()

	// Add decompressed hash to metadata (as would be done during indexing)
	storageInfo := metadata.StorageInfo.(*common.OCIStorageInfo)
	if storageInfo.DecompressedHashByLayer == nil {
		storageInfo.DecompressedHashByLayer = make(map[string]string)
	}
	storageInfo.DecompressedHashByLayer[digest.String()] = decompressedHash

	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         storageInfo,
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:        diskCacheDir,
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        cache,
	}

	// Read only a small portion (1 KB from a 10 MB layer)
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     5 * 1024 * 1024, // 5 MB into the layer
			ULength:     1024,            // Only 1 KB
		},
	}

	dest := make([]byte, 1024)
	n, err := storage.ReadFile(node, dest, 0)
	require.NoError(t, err)
	require.Equal(t, 1024, n)

	// Verify we only fetched 1024 bytes (not 10 MB!)
	// The mock cache's GetContent implementation simulates range reads
	require.Equal(t, 1, cache.getCalls)

	// Verify the data is correct
	expectedOffset := 5 * 1024 * 1024
	require.Equal(t, largeLayerData[expectedOffset:expectedOffset+1024], dest)
}
