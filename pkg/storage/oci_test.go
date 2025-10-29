package storage

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock ContentCache for testing
type mockCache struct {
	mu    sync.Mutex
	store map[string][]byte
	
	// Error injection
	getError error
	setError error
	
	// Call tracking
	getCalls int
	setCalls int
}

func newMockCache() *mockCache {
	return &mockCache{
		store: make(map[string][]byte),
	}
}

func (m *mockCache) Get(key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.getCalls++
	
	if m.getError != nil {
		return nil, false, m.getError
	}
	
	data, found := m.store[key]
	return data, found, nil
}

func (m *mockCache) Set(key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.setCalls++
	
	if m.setError != nil {
		return m.setError
	}
	
	m.store[key] = data
	return nil
}

func (m *mockCache) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.store = make(map[string][]byte)
	m.getCalls = 0
	m.setCalls = 0
	m.getError = nil
	m.setError = nil
}

// Mock Layer for testing
type mockLayer struct {
	digest         v1.Hash
	compressedData []byte
	fetchError     error
}

func (m *mockLayer) Digest() (v1.Hash, error) {
	return m.digest, nil
}

func (m *mockLayer) DiffID() (v1.Hash, error) {
	return m.digest, nil
}

func (m *mockLayer) Compressed() (io.ReadCloser, error) {
	if m.fetchError != nil {
		return nil, m.fetchError
	}
	return io.NopCloser(bytes.NewReader(m.compressedData)), nil
}

func (m *mockLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (m *mockLayer) Size() (int64, error) {
	return int64(len(m.compressedData)), nil
}

func (m *mockLayer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

// Helper to create gzip-compressed test data
func createGzipData(t *testing.T, data []byte) []byte {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	_, err := gzw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func TestOCIStorage_CacheHit(t *testing.T) {
	// Create test data
	testData := []byte("Hello, World! This is test data for OCI storage.")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
	}
	
	// Setup mock cache with data already cached
	cache := newMockCache()
	cacheKey := "clip:oci:layer:" + digest.String()
	cache.store[cacheKey] = compressedData
	
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
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
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
	
	// Read data
	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)
	
	// Assertions
	require.NoError(t, err)
	assert.Equal(t, len(testData), n)
	assert.Equal(t, testData, dest)
	
	// Verify cache was hit (Get called, Set not called)
	assert.Equal(t, 1, cache.getCalls, "cache.Get should be called once")
	assert.Equal(t, 0, cache.setCalls, "cache.Set should not be called on cache hit")
}

func TestOCIStorage_CacheMiss(t *testing.T) {
	// Create test data
	testData := []byte("Hello, World! This is test data for OCI storage.")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
	}
	
	// Setup empty cache
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
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
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
	
	// Read data
	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)
	
	// Assertions
	require.NoError(t, err)
	assert.Equal(t, len(testData), n)
	assert.Equal(t, testData, dest)
	
	// Verify cache miss flow (Get called, Set called asynchronously)
	assert.Equal(t, 1, cache.getCalls, "cache.Get should be called once")
	// Note: Set is async, so we can't reliably assert it here
}

func TestOCIStorage_NoCache(t *testing.T) {
	// Create test data
	testData := []byte("Hello, World! This is test data for OCI storage.")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
	}
	
	// Create mock layer
	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}
	
	// Create storage WITHOUT cache
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {},
			},
		},
	}
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        nil, // No cache
	}
	
	// Create node
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}
	
	// Read data
	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)
	
	// Assertions
	require.NoError(t, err)
	assert.Equal(t, len(testData), n)
	assert.Equal(t, testData, dest)
}

func TestOCIStorage_PartialRead(t *testing.T) {
	// Create test data
	testData := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
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
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
		layersDecompressing: make(map[string]chan struct{}),
		contentCache:        cache,
	}
	
	// Test reading from different offsets
	testCases := []struct {
		name     string
		offset   int64
		length   int
		expected string
	}{
		{"Start", 0, 10, "0123456789"},
		{"Middle", 10, 10, "ABCDEFGHIJ"},
		{"End", 26, 10, "QRSTUVWXYZ"},
		{"Small", 5, 3, "567"},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := &common.ClipNode{
				Remote: &common.RemoteRef{
					LayerDigest: digest.String(),
					UOffset:     0,
					ULength:     int64(len(testData)),
				},
			}
			
			dest := make([]byte, tc.length)
			n, err := storage.ReadFile(node, dest, tc.offset)
			
			require.NoError(t, err)
			assert.Equal(t, tc.length, n)
			assert.Equal(t, tc.expected, string(dest))
		})
	}
}

func TestOCIStorage_CacheError(t *testing.T) {
	// Create test data
	testData := []byte("Hello, World! This is test data for OCI storage.")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
	}
	
	// Setup cache with error injection
	cache := newMockCache()
	cache.getError = errors.New("cache get error")
	
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
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
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
	
	// Read should still succeed (graceful degradation)
	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)
	
	// Assertions
	require.NoError(t, err, "read should succeed even with cache error")
	assert.Equal(t, len(testData), n)
	assert.Equal(t, testData, dest)
}

func TestOCIStorage_LayerFetchError(t *testing.T) {
	// Create test data
	testData := []byte("Hello, World!")
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
	}
	
	// Setup cache
	cache := newMockCache()
	
	// Create mock layer with fetch error
	layer := &mockLayer{
		digest:     digest,
		fetchError: errors.New("network error"),
	}
	
	// Create storage
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {},
			},
		},
	}
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
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
	
	// Read should fail
	dest := make([]byte, len(testData))
	_, err := storage.ReadFile(node, dest, 0)
	
	// Assertions
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network error")
}

func TestOCIStorage_ConcurrentReads(t *testing.T) {
	// Create test data
	testData := []byte("Hello, World! This is test data for concurrent reads.")
	compressedData := createGzipData(t, testData)
	
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
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
	
	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		decompressedLayers:  make(map[string][]byte),
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
	
	// Run concurrent reads
	numReads := 10
	var wg sync.WaitGroup
	wg.Add(numReads)
	
	errors := make(chan error, numReads)
	
	for i := 0; i < numReads; i++ {
		go func() {
			defer wg.Done()
			
			dest := make([]byte, len(testData))
			n, err := storage.ReadFile(node, dest, 0)
			
			if err != nil {
				errors <- err
				return
			}
			
			if n != len(testData) {
				errors <- fmt.Errorf("expected %d bytes, got %d", len(testData), n)
				return
			}
			
			if !bytes.Equal(testData, dest) {
				errors <- fmt.Errorf("data mismatch")
				return
			}
		}()
	}
	
	wg.Wait()
	close(errors)
	
	// Check for errors
	for err := range errors {
		t.Errorf("Concurrent read error: %v", err)
	}
}
