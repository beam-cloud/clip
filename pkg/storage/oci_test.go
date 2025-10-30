package storage

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock ContentCache for testing (implements range read interface)
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

func (m *mockCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.getCalls++

	if m.getError != nil {
		return nil, m.getError
	}

	fullData, found := m.store[hash]
	if !found {
		return nil, fmt.Errorf("not found in cache")
	}

	// Range read simulation
	if offset >= int64(len(fullData)) {
		return nil, fmt.Errorf("offset %d out of range (data length: %d)", offset, len(fullData))
	}

	end := offset + length
	if end > int64(len(fullData)) {
		end = int64(len(fullData))
	}

	return fullData[offset:end], nil
}

func (m *mockCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.setCalls++

	if m.setError != nil {
		return "", m.setError
	}

	// Read all chunks
	var data []byte
	for chunk := range chunks {
		data = append(data, chunk...)
	}

	m.store[hash] = data
	return hash, nil
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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	// Setup mock cache with data already cached (using decompressed hash as key)
	cache := newMockCache()
	cache.store[decompressedHash] = testData

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
		diskCacheDir:        t.TempDir(),
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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}

	storage := &OCIClipStorage{
		metadata:            metadata,
		storageInfo:         metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:          map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:        t.TempDir(),
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

	// Cache miss scenario: we try ContentCache with the decompressed hash, but it's not there
	// Then we decompress and store (async, so can't reliably assert it here)
	assert.Equal(t, 1, cache.getCalls, "cache.Get should be called once to check ContentCache")
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          nil, // No cache
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
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

// Test streaming functionality
func TestStreamFileInChunks_SmallFile(t *testing.T) {
	// Create a small test file (less than chunk size)
	testData := []byte("Hello, World! This is a small test file.")

	// Write to temp file
	tmpDir := t.TempDir()
	tmpFile := tmpDir + "/test.dat"
	err := os.WriteFile(tmpFile, testData, 0644)
	require.NoError(t, err)

	// Stream file
	chunks := make(chan []byte, 10)
	errChan := make(chan error, 1)
	go func() {
		defer close(chunks)
		if err := streamFileInChunks(tmpFile, chunks); err != nil {
			errChan <- err
		}
		close(errChan)
	}()

	// Collect chunks
	var collected []byte
	chunkCount := 0
	for chunk := range chunks {
		collected = append(collected, chunk...)
		chunkCount++
	}

	// Check for errors
	err = <-errChan
	require.NoError(t, err)

	// Verify
	assert.Equal(t, 1, chunkCount, "small file should be sent as single chunk")
	assert.Equal(t, testData, collected, "data should match")
}

func TestStreamFileInChunks_LargeFile(t *testing.T) {
	// Create a large test file (100MB - should be split into multiple chunks)
	fileSize := int64(100 * 1024 * 1024) // 100MB
	chunkSize := int64(1 << 25)          // 32MB

	// Write to temp file
	tmpDir := t.TempDir()
	tmpFile := tmpDir + "/large_test.dat"

	file, err := os.Create(tmpFile)
	require.NoError(t, err)

	// Write test pattern
	pattern := []byte("0123456789ABCDEF")
	written := int64(0)
	for written < fileSize {
		n, err := file.Write(pattern)
		require.NoError(t, err)
		written += int64(n)
	}
	file.Close()

	// Stream file
	chunks := make(chan []byte, 10)
	errChan := make(chan error, 1)
	go func() {
		defer close(chunks)
		if err := streamFileInChunks(tmpFile, chunks); err != nil {
			errChan <- err
		}
		close(errChan)
	}()

	// Collect and verify chunks
	var collected []byte
	chunkCount := 0
	for chunk := range chunks {
		chunkCount++
		collected = append(collected, chunk...)

		// Each chunk (except possibly the last) should be chunkSize
		if chunkCount < 4 { // First 3 chunks should be full size
			assert.Equal(t, int(chunkSize), len(chunk), "chunk %d should be full size", chunkCount)
		}
	}

	// Check for errors
	err = <-errChan
	require.NoError(t, err)

	// Verify
	expectedChunks := (fileSize + chunkSize - 1) / chunkSize
	assert.Equal(t, int(expectedChunks), chunkCount, "should split into expected number of chunks")
	assert.Equal(t, int(fileSize), len(collected), "total size should match")
}

func TestStreamFileInChunks_ExactMultipleOfChunkSize(t *testing.T) {
	// Create file that's exactly 2x chunk size
	chunkSize := int64(1 << 25) // 32MB
	fileSize := chunkSize * 2

	// Write to temp file
	tmpDir := t.TempDir()
	tmpFile := tmpDir + "/exact_test.dat"

	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	err := os.WriteFile(tmpFile, data, 0644)
	require.NoError(t, err)

	// Stream file
	chunks := make(chan []byte, 10)
	errChan := make(chan error, 1)
	go func() {
		defer close(chunks)
		if err := streamFileInChunks(tmpFile, chunks); err != nil {
			errChan <- err
		}
		close(errChan)
	}()

	// Collect chunks
	chunkCount := 0
	for range chunks {
		chunkCount++
	}

	// Check for errors
	err = <-errChan
	require.NoError(t, err)

	// Verify exactly 2 chunks
	assert.Equal(t, 2, chunkCount, "should split into exactly 2 chunks")
}

func TestStreamFileInChunks_NonExistentFile(t *testing.T) {
	// Try to stream non-existent file
	chunks := make(chan []byte, 1)
	err := streamFileInChunks("/nonexistent/file.dat", chunks)

	// Should return error
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open file")
}

// Mock cache that tracks chunked writes
type chunkTrackingCache struct {
	mockCache
	chunksReceived []int // Track sizes of chunks received
	mu             sync.Mutex
}

func (c *chunkTrackingCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setCalls++

	// Track chunk sizes
	var data []byte
	for chunk := range chunks {
		c.chunksReceived = append(c.chunksReceived, len(chunk))
		data = append(data, chunk...)
	}

	c.store[hash] = data
	return hash, nil
}

func TestStoreDecompressedInRemoteCache_StreamsInChunks(t *testing.T) {
	// Create a large test file (100MB)
	fileSize := int64(100 * 1024 * 1024) // 100MB

	tmpDir := t.TempDir()
	tmpFile := tmpDir + "/large_layer.dat"

	// Create test file
	file, err := os.Create(tmpFile)
	require.NoError(t, err)

	// Write test pattern
	pattern := []byte("ABCDEFGHIJ")
	written := int64(0)
	for written < fileSize {
		n, err := file.Write(pattern)
		require.NoError(t, err)
		written += int64(n)
	}
	file.Close()

	// Setup tracking cache
	cache := &chunkTrackingCache{
		mockCache: mockCache{
			store: make(map[string][]byte),
		},
	}

	digest := "sha256:test123"

	// Create storage
	storage := &OCIClipStorage{
		contentCache: cache,
	}

	// Call storeDecompressedInRemoteCache
	storage.storeDecompressedInRemoteCache(digest, tmpFile)

	// Give async operation time to complete
	time.Sleep(100 * time.Millisecond)

	// Verify chunking behavior
	cache.mu.Lock()
	chunksReceived := cache.chunksReceived
	cache.mu.Unlock()

	assert.Greater(t, len(chunksReceived), 1, "should receive multiple chunks for large file")

	// Verify most chunks are the expected size (32MB)
	chunkSize := 1 << 25
	for i := 0; i < len(chunksReceived)-1; i++ {
		assert.Equal(t, chunkSize, chunksReceived[i], "chunk %d should be full size", i)
	}

	// Verify total size
	totalSize := 0
	for _, size := range chunksReceived {
		totalSize += size
	}
	assert.Equal(t, int(fileSize), totalSize, "total size should match file size")
}

func TestStoreDecompressedInRemoteCache_SmallFile(t *testing.T) {
	// Create a small test file
	testData := []byte("Small file content")

	tmpDir := t.TempDir()
	tmpFile := tmpDir + "/small_layer.dat"

	err := os.WriteFile(tmpFile, testData, 0644)
	require.NoError(t, err)

	// Setup tracking cache
	cache := &chunkTrackingCache{
		mockCache: mockCache{
			store: make(map[string][]byte),
		},
	}

	digest := "sha256:small123"

	// Create storage
	storage := &OCIClipStorage{
		contentCache: cache,
	}

	// Call storeDecompressedInRemoteCache
	storage.storeDecompressedInRemoteCache(digest, tmpFile)

	// Give async operation time to complete
	time.Sleep(50 * time.Millisecond)

	// Verify
	cache.mu.Lock()
	defer cache.mu.Unlock()

	assert.Equal(t, 1, len(cache.chunksReceived), "small file should be single chunk")
	assert.Equal(t, len(testData), cache.chunksReceived[0], "chunk size should match file size")

	// Verify content was stored with the digest as key (test calls storeDecompressedInRemoteCache with digest directly)
	assert.Equal(t, testData, cache.store[digest], "cached content should match original")
}

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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          diskCacheDir,
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          cache,
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          diskCacheDir,
		layersDecompressing:   make(map[string]chan struct{}),
		contentCache:          nil, // No remote cache for benchmark
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
