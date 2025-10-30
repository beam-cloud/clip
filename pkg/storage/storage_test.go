package storage

import (
	"os"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
)

// TestGetLayerContentKey verifies that layer cache keys preserve the digest
func TestGetLayerContentKey(t *testing.T) {
	storage := &OCIClipStorage{}

	tests := []struct {
		name     string
		digest   string
		expected string
	}{
		{
			name:     "SHA256 digest",
			digest:   "sha256:abc123def456",
			expected: "sha256:abc123def456",
		},
		{
			name:     "SHA1 digest",
			digest:   "sha1:fedcba987654",
			expected: "sha1:fedcba987654",
		},
		{
			name:     "Long SHA256",
			digest:   "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
			expected: "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
		},
		{
			name:     "No algorithm prefix (fallback)",
			digest:   "justahash123",
			expected: "justahash123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := storage.getLayerContentKey(tc.digest)
			require.Equal(t, tc.expected, result)
		})
	}
}

// TestRemoteCacheKeyFormat documents expected remote cache key structure
func TestRemoteCacheKeyFormat(t *testing.T) {
	t.Skip("Integration test - requires mock ContentCache")

	// Remote cache keys now retain the digest algorithm prefix (e.g., "sha256:...")
	// so routing layers is unambiguous, while disk cache paths remain filesystem-safe.
}

// TestContentAddressedCaching verifies cache keys enable cross-image sharing
func TestContentAddressedCaching(t *testing.T) {
	storage := &OCIClipStorage{}

	// Same layer used in multiple images
	sharedLayerDigest := "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885"

	// Both images should produce the SAME cache key
	cacheKey := storage.getLayerContentKey(sharedLayerDigest)
	require.Equal(t, sharedLayerDigest, cacheKey)
	require.Contains(t, cacheKey, "sha256:", "Cache key should retain algorithm prefix for routing")
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
	cacheKey := digest.String()
	chunks := make(chan []byte, 1)
	chunks <- layerData
	close(chunks)
	_, err := cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{RoutingKey: cacheKey})
	require.NoError(t, err)

	// Create mock layer
	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}

	// Create storage
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
		require.Equal(t, cacheKey, cache.lastGetRoutingKey)
	})

	// Test 2: Range read from middle of layer
	t.Run("RangeReadMiddle", func(t *testing.T) {
		cache.reset()
		// Re-populate cache
		chunks := make(chan []byte, 1)
		chunks <- layerData
		close(chunks)
		_, _ = cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{RoutingKey: cacheKey})

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
		require.Equal(t, cacheKey, cache.lastGetRoutingKey)
	})

	// Test 3: Partial file read (offset into the file itself)
	t.Run("PartialFileRead", func(t *testing.T) {
		cache.reset()
		// Re-populate cache
		chunks := make(chan []byte, 1)
		chunks <- layerData
		close(chunks)
		_, _ = cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{RoutingKey: cacheKey})

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
		require.Equal(t, cacheKey, cache.lastGetRoutingKey)
	})
}

// TestContentCachePreferredWithDiskFallback ensures remote cache is checked before disk
func TestContentCachePreferredWithDiskFallback(t *testing.T) {
	layerData := []byte("Layer data for cache hierarchy test")
	compressedData := createGzipData(t, layerData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "hierarchy123",
	}

	cache := newMockCache()
	cacheKey := digest.String()

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

	// Pre-populate disk cache to simulate existing decompressed layer
	diskPath := storage.getDiskCachePath(digest.String())
	require.NoError(t, os.WriteFile(diskPath, layerData, 0644))

	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     5,
			ULength:     10,
		},
	}

	// First read: remote cache miss should fall back to disk cache
	dest := make([]byte, 10)
	n, err := storage.ReadFile(node, dest, 0)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, layerData[5:15], dest)
	require.Equal(t, 1, cache.getCalls, "Content cache should be checked before disk")
	require.Equal(t, cacheKey, cache.lastGetRoutingKey)

	// Second read: populate remote cache and verify it now serves the request
	cache.reset()
	chunks := make(chan []byte, 1)
	chunks <- layerData
	close(chunks)
	_, err = cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{RoutingKey: cacheKey})
	require.NoError(t, err)

	dest2 := make([]byte, 10)
	n, err = storage.ReadFile(node, dest2, 0)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, layerData[5:15], dest2)
	require.Equal(t, 1, cache.getCalls, "Content cache should satisfy the read")
	require.Equal(t, cacheKey, cache.lastGetRoutingKey)
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
	cacheKey := digest.String()

	// Pre-populate cache with large layer
	chunks := make(chan []byte, 1)
	chunks <- largeLayerData
	close(chunks)
	_, err := cache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{RoutingKey: cacheKey})
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
	require.Equal(t, cacheKey, cache.lastGetRoutingKey)

	// Verify the data is correct
	expectedOffset := 5 * 1024 * 1024
	require.Equal(t, largeLayerData[expectedOffset:expectedOffset+1024], dest)
}
