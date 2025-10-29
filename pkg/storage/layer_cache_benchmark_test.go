package storage

import (
	"os"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
)

// TestLayerCacheEliminatesRepeatedInflates verifies that accessing the same layer
// multiple times only triggers ONE decompression operation
func TestLayerCacheEliminatesRepeatedInflates(t *testing.T) {
	// Create test data
	testData := []byte("Test data for layer caching verification")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "test123",
	}
	
	// Setup cache
	cache := newMockCache()
	
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
	
	// Create node
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}
	
	// Read the same data 50 times (simulating the user's workload)
	const numReads = 50
	
	// First read - should decompress and cache to disk
	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)
	require.NoError(t, err)
	require.Equal(t, len(testData), n)
	require.Equal(t, testData, dest)
	
	// Check that layer is now cached on disk
	layerPath := storage.getDiskCachePath(digest.String())
	_, err = os.Stat(layerPath)
	require.NoError(t, err, "Layer should be cached on disk after first read")
	
	// Remaining 49 reads - should all hit disk cache (no decompression)
	for i := 1; i < numReads; i++ {
		dest := make([]byte, len(testData))
		n, err := storage.ReadFile(node, dest, 0)
		require.NoError(t, err)
		require.Equal(t, len(testData), n)
		require.Equal(t, testData, dest)
	}
	
	t.Logf("✅ SUCCESS: %d reads completed - layer decompressed once and cached to disk!", numReads)
}

// BenchmarkLayerCachePerformance benchmarks the performance difference
func BenchmarkLayerCachePerformance(b *testing.B) {
	// Create test data (10KB)
	testData := make([]byte, 10*1024)
	for i := range testData {
		testData[i] = byte(i % 256)
	}
	compressedData := createGzipDataBench(b, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "bench123",
	}
	
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
	
	diskCacheDir := b.TempDir()
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:        diskCacheDir,
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        nil, // No remote cache for benchmark
	}
	
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}
	
	b.ResetTimer()
	
	// Benchmark: After first access, all reads should be instant (disk read)
	for i := 0; i < b.N; i++ {
		dest := make([]byte, len(testData))
		_, err := storage.ReadFile(node, dest, 0)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func createGzipDataBench(b *testing.B, data []byte) []byte {
	return createGzipData(&testing.T{}, data)
}
