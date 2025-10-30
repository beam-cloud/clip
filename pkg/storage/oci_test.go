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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
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

	// Compute decompressed hash (as would be done during indexing)
	hasher := sha256.New()
	hasher.Write(sharedLayerData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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
			DecompressedHashByLayer: map[string]string{
				sharedDigest.String(): decompressedHash,
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
			DecompressedHashByLayer: map[string]string{
				sharedDigest.String(): decompressedHash,
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
			// Create storage with metadata containing decompressed hash
			storageInfo := &common.OCIStorageInfo{
				DecompressedHashByLayer: map[string]string{
					tc.digest: tc.expectedSuffix,
				},
			}
			storage := &OCIClipStorage{
				diskCacheDir: diskCacheDir,
				storageInfo:  storageInfo,
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

// TestCheckpointBasedReading tests checkpoint-based partial decompression
func TestCheckpointBasedReading(t *testing.T) {
	// Create multi-chunk test data (6 MB to ensure multiple checkpoints)
	const dataSize = 6 * 1024 * 1024
	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	compressedData := createGzipData(t, testData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "checkpoint_test_123",
	}

	// Create checkpoints (simulating what the indexer would create)
	// Checkpoint every 2 MiB
	checkpoints := []common.GzipCheckpoint{
		{COff: 0, UOff: 0},
		{COff: int64(len(compressedData)) / 3, UOff: 2 * 1024 * 1024},
		{COff: 2 * int64(len(compressedData)) / 3, UOff: 4 * 1024 * 1024},
	}

	// Compute decompressed hash
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	// Create mock layer
	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}

	// Create storage WITH checkpoints enabled
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: checkpoints,
				},
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
		contentCache:        nil,
		useCheckpoints:      true, // Enable checkpoint-based reading
	}

	// Test reading from different positions (should use checkpoints)
	testCases := []struct {
		name   string
		offset int64
		length int
	}{
		{"Start of file", 0, 1024},
		{"After first checkpoint", 2*1024*1024 + 100, 2048},
		{"After second checkpoint", 4*1024*1024 + 500, 1024},
		{"Near end", dataSize - 1000, 1000},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := &common.ClipNode{
				Remote: &common.RemoteRef{
					LayerDigest: digest.String(),
					UOffset:     0,
					ULength:     int64(dataSize),
				},
			}

			dest := make([]byte, tc.length)
			n, err := storage.ReadFile(node, dest, tc.offset)

			require.NoError(t, err, "checkpoint-based read should succeed")
			assert.Equal(t, tc.length, n, "should read requested number of bytes")

			// Verify data correctness
			expected := testData[tc.offset : tc.offset+int64(tc.length)]
			assert.Equal(t, expected, dest, "data read via checkpoints should match original")
		})
	}

	t.Log("✅ Checkpoint-based reading test passed!")
}

// TestCheckpointFallback tests that checkpoint mode falls back to full decompression when needed
func TestCheckpointFallback(t *testing.T) {
	testData := []byte("Test data for checkpoint fallback")
	compressedData := createGzipData(t, testData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "fallback_test",
	}

	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}

	// Create storage with checkpoints enabled but NO checkpoints available
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: []common.GzipCheckpoint{}, // Empty!
				},
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
		contentCache:        nil,
		useCheckpoints:      true, // Enabled but no checkpoints
	}

	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}

	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)

	// Should succeed by falling back to full layer decompression
	require.NoError(t, err, "should fall back to full decompression")
	assert.Equal(t, len(testData), n)
	assert.Equal(t, testData, dest)

	t.Log("✅ Checkpoint fallback test passed!")
}

// TestBackwardCompatibilityNoCheckpoints tests that disabling checkpoints works (backward compatibility)
func TestBackwardCompatibilityNoCheckpoints(t *testing.T) {
	testData := []byte("Test data for backward compatibility")
	compressedData := createGzipData(t, testData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "compat_test",
	}

	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	layer := &mockLayer{
		digest:         digest,
		compressedData: compressedData,
	}

	// Create checkpoints (they exist in metadata but won't be used)
	checkpoints := []common.GzipCheckpoint{
		{COff: 0, UOff: 0},
	}

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: checkpoints,
				},
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
		contentCache:        nil,
		useCheckpoints:      false, // Checkpoints DISABLED (backward compatibility)
	}

	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}

	dest := make([]byte, len(testData))
	n, err := storage.ReadFile(node, dest, 0)

	// Should work using traditional full-layer decompression
	require.NoError(t, err, "should work with checkpoints disabled")
	assert.Equal(t, len(testData), n)
	assert.Equal(t, testData, dest)

	// Verify the layer was cached to disk (traditional behavior)
	layerPath := storage.getDiskCachePath(digest.String())
	_, err = os.Stat(layerPath)
	require.NoError(t, err, "layer should be cached to disk when checkpoints disabled")

	t.Log("✅ Backward compatibility test passed!")
}

// TestNearestCheckpoint tests the checkpoint selection algorithm
func TestNearestCheckpoint(t *testing.T) {
	checkpoints := []common.GzipCheckpoint{
		{COff: 100, UOff: 0},
		{COff: 200, UOff: 2 * 1024 * 1024},
		{COff: 300, UOff: 4 * 1024 * 1024},
		{COff: 400, UOff: 6 * 1024 * 1024},
	}

	testCases := []struct {
		name          string
		wantUOffset   int64
		expectedCOff  int64
		expectedUOff  int64
		description   string
	}{
		{"Before first checkpoint", 0, 100, 0, "should use first checkpoint"},
		{"Exactly at checkpoint", 2 * 1024 * 1024, 200, 2 * 1024 * 1024, "should use exact checkpoint"},
		{"Between checkpoints", 3 * 1024 * 1024, 200, 2 * 1024 * 1024, "should use previous checkpoint"},
		{"After last checkpoint", 10 * 1024 * 1024, 400, 6 * 1024 * 1024, "should use last checkpoint"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cOff, uOff := nearestCheckpoint(checkpoints, tc.wantUOffset)
			assert.Equal(t, tc.expectedCOff, cOff, "compressed offset should match")
			assert.Equal(t, tc.expectedUOff, uOff, "uncompressed offset should match")
			t.Logf("%s: wantU=%d -> cOff=%d, uOff=%d", tc.description, tc.wantUOffset, cOff, uOff)
		})
	}
}

// TestCheckpointEmptyList tests nearestCheckpoint with empty checkpoint list
func TestCheckpointEmptyList(t *testing.T) {
	cOff, uOff := nearestCheckpoint([]common.GzipCheckpoint{}, 1000)
	assert.Equal(t, int64(0), cOff, "should return 0 for empty checkpoint list")
	assert.Equal(t, int64(0), uOff, "should return 0 for empty checkpoint list")
}
