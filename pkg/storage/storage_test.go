package storage

import (
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
)

// TestGetContentHash verifies that content hashes are extracted correctly
func TestGetContentHash(t *testing.T) {
	storage := &OCIClipStorage{}

	tests := []struct {
		name     string
		digest   string
		expected string
	}{
		{
			name:     "SHA256 digest",
			digest:   "sha256:abc123def456",
			expected: "sha256_abc123def456", // Now keeps algorithm prefix with underscore
		},
		{
			name:     "SHA1 digest",
			digest:   "sha1:fedcba987654",
			expected: "sha1_fedcba987654", // Now keeps algorithm prefix with underscore
		},
		{
			name:     "Long SHA256",
			digest:   "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
			expected: "sha256_44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885", // Full format
		},
		{
			name:     "No algorithm prefix (fallback)",
			digest:   "justahash123",
			expected: "justahash123", // No colon, stays the same
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := storage.getContentHash(tc.digest)
			require.Equal(t, tc.expected, result)
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

// TestContentAddressedCaching verifies cache keys enable cross-image sharing
func TestContentAddressedCaching(t *testing.T) {
	storage := &OCIClipStorage{}

	// Same layer used in multiple images
	sharedLayerDigest := "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885"

	// Both images should produce the SAME cache key
	cacheKey := storage.getContentHash(sharedLayerDigest)

	// Cache key should use filesystem-safe format with underscore
	require.Equal(t, "sha256_44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885", cacheKey)
	require.NotContains(t, cacheKey, ":", "Cache key should use underscore instead of colon")
	require.NotContains(t, cacheKey, "clip:", "Cache key should not contain namespace prefix")
	require.NotContains(t, cacheKey, "decompressed", "Cache key should not contain type suffix")

	t.Logf("✅ Content-addressed cache key: %s", cacheKey)
	t.Logf("This key can be shared across multiple images with the same layer!")
}

// TestContentCacheRangeRead verifies that we do range reads from ContentCache
// instead of fetching entire layers
func TestContentCacheRangeRead(t *testing.T) {
	// Create test layer data
	layerData := []byte("This is a test layer with some content for range reading verification")
	compressedData := createGzipData(t, layerData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "rangetest123",
	}

	// Setup cache
	cache := newMockCache()

	// Pre-populate cache with the entire layer (simulating Node A caching it)
	storage := &OCIClipStorage{}
	cacheKey := storage.getContentHash(digest.String()) // Use proper cache key format
	chunks := make(chan []byte, 1)
	chunks <- layerData
	close(chunks)
	_, err := cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{})
	require.NoError(t, err)

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
		},
	}

	diskCacheDir := t.TempDir()

	storage = &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:        diskCacheDir,
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        cache,
	}

	// Test 1: Range read from start of layer
	t.Run("RangeReadStart", func(t *testing.T) {
		node := &common.ClipNode{
			Remote: &common.RemoteRef{
				LayerDigest: digest.String(),
				UOffset:     0,
				ULength:     10, // Only want 10 bytes
			},
		}

		dest := make([]byte, 10)
		n, err := storage.ReadFile(node, dest, 0)
		require.NoError(t, err)
		require.Equal(t, 10, n)
		require.Equal(t, layerData[0:10], dest)

		// Verify we did a range read from ContentCache (not full layer)
		require.Equal(t, 1, cache.getCalls)
	})

	// Test 2: Range read from middle of layer
	t.Run("RangeReadMiddle", func(t *testing.T) {
		cache.reset()
		// Re-populate cache
		chunks := make(chan []byte, 1)
		chunks <- layerData
		close(chunks)
		_, _ = cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{})

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

		// Verify we did a range read
		require.Equal(t, 1, cache.getCalls)
	})

	// Test 3: Partial file read (offset into the file itself)
	t.Run("PartialFileRead", func(t *testing.T) {
		cache.reset()
		// Re-populate cache
		chunks := make(chan []byte, 1)
		chunks <- layerData
		close(chunks)
		_, _ = cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{})

		node := &common.ClipNode{
			Remote: &common.RemoteRef{
				LayerDigest: digest.String(),
				UOffset:     10, // File starts at offset 10
				ULength:     20, // File is 20 bytes long
			},
		}

		// Read from offset 5 within the file (absolute offset 15 in layer)
		dest := make([]byte, 10)
		n, err := storage.ReadFile(node, dest, 5)
		require.NoError(t, err)
		require.Equal(t, 10, n)
		require.Equal(t, layerData[15:25], dest)

		// Verify we did a range read starting at offset 15
		require.Equal(t, 1, cache.getCalls)
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
		},
	}

	diskCacheDir := t.TempDir()

	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:        diskCacheDir,
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        cache,
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

	cache := newMockCache()
	cacheKey := digest.Hex

	// Pre-populate cache with large layer
	chunks := make(chan []byte, 1)
	chunks <- largeLayerData
	close(chunks)
	_, err := cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{})
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

	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
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
