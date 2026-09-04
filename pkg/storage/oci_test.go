package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	mu                       sync.Mutex
	store                    map[string][]byte
	clientLocalPageFileViews map[string][]ClientLocalPageFileView

	// Error injection
	getError error
	setError error

	// Call tracking
	getCalls                      int
	setCalls                      int
	getOffsets                    []int64
	getLengths                    []int64
	clientLocalPageFileViewCalls  int
	clientLocalPageFileViewOffset int64
	clientLocalPageFileViewLength int64
}

func newMockCache() *mockCache {
	return &mockCache{
		store:                    make(map[string][]byte),
		clientLocalPageFileViews: make(map[string][]ClientLocalPageFileView),
	}
}

type streamingMockCache struct {
	mockCache
	streamCalls         int
	existsWithSizeCalls int
	nilStreamHash       string
	nilStreamSize       int64
}

func (m *streamingMockCache) GetContentStream(hash string, _ struct{ RoutingKey string }) (<-chan []byte, int64, error) {
	m.mu.Lock()
	m.streamCalls++
	if hash == m.nilStreamHash {
		size := m.nilStreamSize
		m.mu.Unlock()
		return nil, size, nil
	}
	data, ok := m.store[hash]
	data = append([]byte(nil), data...)
	m.mu.Unlock()
	if !ok {
		return nil, 0, ErrContentCacheMiss
	}

	chunks := make(chan []byte, 2)
	go func() {
		defer close(chunks)
		mid := len(data) / 2
		if mid > 0 {
			chunks <- data[:mid]
		}
		chunks <- data[mid:]
	}()
	return chunks, int64(len(data)), nil
}

func (m *streamingMockCache) ContentExistsWithSize(hash string, size int64, _ struct{ RoutingKey string }) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.existsWithSizeCalls++
	data, ok := m.store[hash]
	return ok && int64(len(data)) == size, nil
}

type scriptedCompressedStreamCache struct {
	mockCache
	key          string
	data         []byte
	reportedSize int64
	exists       bool
	started      chan struct{}
	release      <-chan struct{}
	streamCalls  int
	nilStream    bool
}

func (m *scriptedCompressedStreamCache) ContentExistsWithSize(hash string, size int64, _ struct{ RoutingKey string }) (bool, error) {
	return m.exists && hash == m.key && size == m.reportedSize, nil
}

func (m *scriptedCompressedStreamCache) GetContentStream(hash string, _ struct{ RoutingKey string }) (<-chan []byte, int64, error) {
	m.mu.Lock()
	m.streamCalls++
	m.mu.Unlock()
	if hash != m.key {
		return nil, 0, ErrContentCacheMiss
	}
	if m.nilStream {
		return nil, m.reportedSize, nil
	}

	chunks := make(chan []byte, 1)
	data := append([]byte(nil), m.data...)
	go func() {
		defer close(chunks)
		if len(data) > 0 {
			chunks <- data
		}
		if m.started != nil {
			close(m.started)
		}
		if m.release != nil {
			<-m.release
		}
	}()
	return chunks, m.reportedSize, nil
}

func (m *scriptedCompressedStreamCache) StreamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streamCalls
}

func (m *mockCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.getCalls++
	m.getOffsets = append(m.getOffsets, offset)
	m.getLengths = append(m.getLengths, length)

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

func (m *mockCache) ClientLocalPageFileViews(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]ClientLocalPageFileView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.clientLocalPageFileViewCalls++
	m.clientLocalPageFileViewOffset = offset
	m.clientLocalPageFileViewLength = length
	views := m.clientLocalPageFileViews[hash]
	if len(views) == 0 {
		return nil, fmt.Errorf("not found in cache")
	}
	return append([]ClientLocalPageFileView(nil), views...), nil
}

func (m *mockCache) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.store = make(map[string][]byte)
	m.clientLocalPageFileViews = make(map[string][]ClientLocalPageFileView)
	m.getCalls = 0
	m.setCalls = 0
	m.getOffsets = nil
	m.getLengths = nil
	m.clientLocalPageFileViewCalls = 0
	m.clientLocalPageFileViewOffset = 0
	m.clientLocalPageFileViewLength = 0
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

type contextBoundLayer struct {
	*mockLayer
	ctx context.Context
}

func (l *contextBoundLayer) Compressed() (io.ReadCloser, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	return l.mockLayer.Compressed()
}

type blockingCountingLayer struct {
	*mockLayer
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
}

func (m *blockingCountingLayer) Compressed() (io.ReadCloser, error) {
	m.mu.Lock()
	m.calls++
	m.once.Do(func() { close(m.started) })
	m.mu.Unlock()

	<-m.release
	return m.mockLayer.Compressed()
}

func (m *blockingCountingLayer) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type blockingCompressedStreamLayer struct {
	*mockLayer
	content []byte
	started chan struct{}
	release chan struct{}
	stopped chan struct{}
}

func (m *blockingCompressedStreamLayer) Compressed() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		defer close(m.stopped)

		gzw := gzip.NewWriter(writer)
		if _, err := gzw.Write(m.content); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if err := gzw.Flush(); err != nil {
			_ = writer.CloseWithError(err)
			return
		}

		close(m.started)
		<-m.release
		if err := gzw.Close(); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return reader, nil
}

type barrierCountingLayer struct {
	*mockLayer
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	calls   int
}

func (m *barrierCountingLayer) Compressed() (io.ReadCloser, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	m.started <- struct{}{}
	<-m.release
	return m.mockLayer.Compressed()
}

func (m *barrierCountingLayer) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type blockAfterFirstCompressedLayer struct {
	*mockLayer
	mu          sync.Mutex
	calls       int
	warmStarted chan struct{}
	warmRelease chan struct{}
	once        sync.Once
}

func (m *blockAfterFirstCompressedLayer) Compressed() (io.ReadCloser, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()

	if call > 1 {
		m.once.Do(func() { close(m.warmStarted) })
		<-m.warmRelease
	}

	return m.mockLayer.Compressed()
}

func (m *blockAfterFirstCompressedLayer) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
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
		metadata:              metadata,
		storageInfo:           storageInfo,
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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

func TestOCIStorage_ClientLocalFileViewUsesDiskCache(t *testing.T) {
	testData := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, decompressedHash), testData, 0644))

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{digest.String(): {}},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	cache := newMockCache()
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir:          cacheDir,
		contentCache:          cache,
		contentCacheAvailable: true,
		readTraceObserver:     func(common.ReadTraceEvent) {},
	}
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     10,
			ULength:     20,
		},
	}

	region, ok, err := storage.ClientLocalFileView(context.Background(), node, 3, 7)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(cacheDir, decompressedHash), region.Path)
	assert.Equal(t, int64(13), region.Offset)
	assert.Equal(t, 7, region.Length)
	assert.Equal(t, "disk_cache_fd", region.Source)
	assert.Equal(t, digest.String(), region.LayerDigest)
	assert.Equal(t, decompressedHash, region.DecompressedHash)
	assert.Equal(t, "hit", region.Attrs["cache_result"])
	assert.Equal(t, "local_decompressed_layer", region.Attrs["cache_tier"])

	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.setCalls == 1
	}, time.Second, 10*time.Millisecond, "disk fd fast path should schedule one remote content cache repair")

	cache.mu.Lock()
	assert.Equal(t, testData, cache.store[decompressedHash])
	cache.mu.Unlock()
}

func TestOCIStorage_ClientLocalFileViewSkipsTraceAttrsWithoutObserver(t *testing.T) {
	testData := []byte("0123456789")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, decompressedHash), testData, 0644))

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
		},
	}
	storage := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir: cacheDir,
	}
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			ULength:     int64(len(testData)),
		},
	}

	region, ok, err := storage.ClientLocalFileView(context.Background(), node, 0, int64(len(testData)))
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, region.Attrs)
}

func TestOCIStorage_ClientLocalFileViewCachesImmutableLayerPresence(t *testing.T) {
	testData := []byte("0123456789")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	cacheDir := t.TempDir()
	layerPath := filepath.Join(cacheDir, decompressedHash)
	require.NoError(t, os.WriteFile(layerPath, testData, 0644))

	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	storage := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir: cacheDir,
	}
	node := &common.ClipNode{Remote: &common.RemoteRef{
		LayerDigest: digest.String(),
		ULength:     int64(len(testData)),
	}}

	first, ok, err := storage.ClientLocalFileView(context.Background(), node, 0, int64(len(testData)))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, os.Remove(layerPath))

	second, ok, err := storage.ClientLocalFileView(context.Background(), node, 0, int64(len(testData)))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first, second)
}

func TestOCIStorage_ClientLocalFileViewWarmsContentCacheOncePerLayer(t *testing.T) {
	testData := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, decompressedHash), testData, 0644))

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{digest.String(): {}},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	cache := &localPathTrackingCache{
		mockCache: mockCache{
			store: make(map[string][]byte),
		},
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir:          cacheDir,
		contentCache:          cache,
		contentCacheAvailable: true,
	}
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}

	for i := 0; i < 5; i++ {
		region, ok, err := storage.ClientLocalFileView(context.Background(), node, int64(i), 1)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "disk_cache_fd", region.Source)
	}

	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.localPathCalls == 1
	}, time.Second, 10*time.Millisecond)

	time.Sleep(25 * time.Millisecond)
	cache.mu.Lock()
	assert.Equal(t, 1, cache.localPathCalls, "local fd reads should not repeatedly enter remote cache local-path store")
	assert.Equal(t, 0, cache.setCalls, "local path store should avoid streaming StoreContent fallback")
	assert.Equal(t, filepath.Join(cacheDir, decompressedHash), cache.localPath)
	assert.Equal(t, decompressedHash, cache.routingKey)
	assert.Equal(t, testData, cache.store[decompressedHash])
	cache.mu.Unlock()
}

func TestOCIStorage_ReadFileDiskCacheHitWarmsContentCache(t *testing.T) {
	testData := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, decompressedHash), testData, 0644))

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{digest.String(): {}},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	cache := newMockCache()
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir:          cacheDir,
		contentCache:          cache,
		contentCacheAvailable: true,
	}
	node := &common.ClipNode{
		Path: "/file",
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}

	dest := make([]byte, 7)
	n, err := storage.ReadFile(node, dest, 3)
	require.NoError(t, err)
	require.Equal(t, 7, n)
	require.Equal(t, testData[3:10], dest)

	cache.mu.Lock()
	assert.Equal(t, 0, cache.getCalls, "disk cache hit should not do a remote range read")
	cache.mu.Unlock()

	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.setCalls == 1
	}, time.Second, 10*time.Millisecond, "disk cache hit should schedule one remote content cache repair")

	cache.mu.Lock()
	assert.Equal(t, testData, cache.store[decompressedHash])
	cache.mu.Unlock()
}

func TestOCIStorage_ClientLocalFileViewUsesContentCachePage(t *testing.T) {
	testData := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))
	pagePath := filepath.Join(t.TempDir(), "page")
	require.NoError(t, os.WriteFile(pagePath, testData, 0644))

	cache := newMockCache()
	cache.clientLocalPageFileViews[decompressedHash] = []ClientLocalPageFileView{{
		Path:   pagePath,
		Offset: 1234,
		Length: 9,
	}}
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{digest.String(): {}},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
		readTraceObserver:     func(common.ReadTraceEvent) {},
	}
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     4096,
			ULength:     64,
		},
	}

	region, ok, err := storage.ClientLocalFileView(context.Background(), node, 5, 9)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, pagePath, region.Path)
	assert.Equal(t, int64(1234), region.Offset)
	assert.Equal(t, 9, region.Length)
	assert.Equal(t, "client_local_page_file_fd", region.Source)
	assert.Equal(t, "hit", region.Attrs["cache_result"])
	assert.Equal(t, "embedded_page_file", region.Attrs["cache_tier"])
	assert.Equal(t, 1, cache.clientLocalPageFileViewCalls)
	assert.Equal(t, int64(4101), cache.clientLocalPageFileViewOffset)
	assert.Equal(t, int64(9), cache.clientLocalPageFileViewLength)
}

func TestOCIStorage_ContentCacheReadAheadCoalescesAdjacentReads(t *testing.T) {
	testData := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])

	cache := newMockCache()
	cache.store[decompressedHash] = testData
	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
		contentCacheReadAhead: NewContentCacheReadAhead(cache, ContentCacheReadAheadOptions{WindowBytes: 16, MaxWindows: 2}),
		layerLimitByHash: map[string]int64{
			decompressedHash: int64(len(testData)),
		},
	}
	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}

	first := make([]byte, 4)
	n, err := storage.ReadFile(node, first, 3)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []byte("3456"), first)

	second := make([]byte, 4)
	n, err = storage.ReadFile(node, second, 10)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []byte("abcd"), second)

	third := make([]byte, 4)
	n, err = storage.ReadFile(node, third, 20)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []byte("klmn"), third)

	// The read at 10 reached the second half of window 0-16 and prefetched
	// 16-32, which the read at 20 then joined instead of fetching again; the
	// read at 20 ends exactly at that window's midpoint, so it prefetches
	// nothing. Every window is fetched exactly once.
	storage.contentCacheReadAhead.WaitPrefetches()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Equal(t, 2, cache.getCalls)
	require.ElementsMatch(t, []int64{0, 16}, cache.getOffsets)
	require.ElementsMatch(t, []int64{16, 16}, cache.getLengths)
}

func TestContentCacheReadAheadPrefetchesNextWindowForSequentialReaders(t *testing.T) {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	cache := newMockCache()
	cache.store["h"] = data
	ra := NewContentCacheReadAhead(cache, ContentCacheReadAheadOptions{WindowBytes: 16, MaxWindows: 4})
	opts := struct{ RoutingKey string }{RoutingKey: "h"}

	// First half of the first window: nothing speculative.
	buf := make([]byte, 4)
	_, err := ra.Read("h", 0, buf, opts, 64)
	require.NoError(t, err)
	ra.WaitPrefetches()
	cache.mu.Lock()
	require.Equal(t, []int64{0}, cache.getOffsets)
	cache.mu.Unlock()

	// Second half: the next window is fetched in the background, once.
	_, err = ra.Read("h", 12, buf, opts, 64)
	require.NoError(t, err)
	_, err = ra.Read("h", 8, buf, opts, 64)
	require.NoError(t, err)
	ra.WaitPrefetches()
	cache.mu.Lock()
	require.ElementsMatch(t, []int64{0, 16}, cache.getOffsets)
	cache.mu.Unlock()

	// The prefetched window is served without another fetch, and reading its
	// tail prefetches the one after it.
	_, err = ra.Read("h", 16, buf, opts, 64)
	require.NoError(t, err)
	require.Equal(t, data[16:20], buf)
	_, err = ra.Read("h", 28, buf, opts, 64)
	require.NoError(t, err)
	ra.WaitPrefetches()
	cache.mu.Lock()
	require.ElementsMatch(t, []int64{0, 16, 32}, cache.getOffsets)
	cache.mu.Unlock()

	// The last window stops at the limit and nothing is fetched past it.
	_, err = ra.Read("h", 60, buf, opts, 64)
	require.NoError(t, err)
	ra.WaitPrefetches()
	cache.mu.Lock()
	require.ElementsMatch(t, []int64{0, 16, 32, 48}, cache.getOffsets)
	require.ElementsMatch(t, []int64{16, 16, 16, 16}, cache.getLengths)
	cache.mu.Unlock()
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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
		metadata:     metadata,
		storageInfo:  metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: t.TempDir(),
		contentCache: nil, // No cache
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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

func TestOCIStorage_ContentCacheUnavailableFallsBackToLayerFetch(t *testing.T) {
	testData := []byte("Hello, World! This is test data for OCI storage.")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "abc123",
	}

	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	cache := newMockCache()
	cache.getError = ErrContentCacheUnavailable
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
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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

	require.NoError(t, err)
	require.Equal(t, len(testData), n)
	require.Equal(t, testData, dest)
	_, statErr := os.Stat(storage.getDecompressedCachePath(decompressedHash))
	require.NoError(t, statErr)
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
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

func TestOCIStorage_GlobalLayerDecompressionSingleflightAcrossInstances(t *testing.T) {
	testData := []byte("shared layer materialization across storage instances")
	compressedData := createGzipData(t, testData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "global-singleflight-layer",
	}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	diskCacheDir := t.TempDir()

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	storageInfo := metadata.StorageInfo.(*common.OCIStorageInfo)
	layer := &blockingCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}

	storage1 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}
	storage2 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}

	errs := make(chan error, 2)
	go func() {
		_, _, err := storage1.ensureLayerCached(context.Background(), digest.String())
		errs <- err
	}()

	<-layer.started
	go func() {
		_, _, err := storage2.ensureLayerCached(context.Background(), digest.String())
		errs <- err
	}()

	close(layer.release)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.Equal(t, 1, layer.Calls(), "only one storage instance should decompress a shared layer")

	got, err := os.ReadFile(filepath.Join(diskCacheDir, decompressedHash))
	require.NoError(t, err)
	require.Equal(t, testData, got)
}

func TestOCIStorage_GlobalLayerDecompressionWaiterHonorsCancellation(t *testing.T) {
	testData := []byte("canceled waiter must not inherit the owner's lifetime")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "global-singleflight-canceled-waiter"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	diskCacheDir := t.TempDir()

	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	storageInfo := metadata.StorageInfo.(*common.OCIStorageInfo)
	layer := &blockingCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseOwner := func() { releaseOnce.Do(func() { close(layer.release) }) }
	defer releaseOwner()

	storage1 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}
	storage2 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}

	ownerDone := make(chan error, 1)
	go func() {
		_, _, err := storage1.ensureLayerCached(context.Background(), digest.String())
		ownerDone <- err
	}()
	select {
	case <-layer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not begin layer decompression")
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := storage2.ensureLayerCached(waiterCtx, digest.String())
		waiterDone <- err
	}()
	cancelWaiter()

	select {
	case err := <-waiterDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter remained blocked on the owner's decompression")
	}
	select {
	case err := <-ownerDone:
		t.Fatalf("owner unexpectedly completed before release: %v", err)
	default:
	}

	releaseOwner()
	select {
	case err := <-ownerDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not complete after release")
	}
	require.Equal(t, 1, layer.Calls())
}

func TestLayerDecompressGroupCanceledOwnerHandsOffToLiveWaiter(t *testing.T) {
	group := newLayerDecompressGroup()
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerStarted := make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		_, err := group.Do(ownerCtx, "layer", func() error {
			close(ownerStarted)
			<-ownerCtx.Done()
			return ownerCtx.Err()
		})
		ownerDone <- err
	}()
	<-ownerStarted

	waiterWork := false
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancelOwner()
	}()

	shared, err := group.Do(context.Background(), "layer", func() error {
		waiterWork = true
		return nil
	})
	require.True(t, shared)
	require.NoError(t, err)
	require.True(t, waiterWork)
	require.ErrorIs(t, <-ownerDone, context.Canceled)
}

func TestOCIStorage_GlobalLayerDecompressionOwnerHonorsCancellation(t *testing.T) {
	testData := bytes.Repeat([]byte("cancel-owner-copy"), 4096)
	digest := v1.Hash{Algorithm: "sha256", Hex: "global-singleflight-canceled-owner"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	diskCacheDir := t.TempDir()
	layerPath := filepath.Join(diskCacheDir, decompressedHash)

	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	layer := &blockingCompressedStreamLayer{
		mockLayer: &mockLayer{digest: digest},
		content:   testData,
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(layer.release) }) }
	defer releaseStream()

	storage := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := storage.ensureLayerCached(ctx, digest.String())
		done <- err
	}()
	select {
	case <-layer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not begin streaming the compressed layer")
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled owner remained blocked in layer decompression")
	}

	releaseStream()
	select {
	case <-layer.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("compressed layer producer did not stop")
	}
	_, err := os.Stat(layerPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	tempFiles, err := filepath.Glob(layerPath + ".*.tmp")
	require.NoError(t, err)
	require.Empty(t, tempFiles)
}

func TestOCIStorage_GlobalLayerDecompressionDoesNotShareDifferentCacheDirs(t *testing.T) {
	testData := []byte("same immutable layer in separate disk cache directories")
	compressedData := createGzipData(t, testData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "global-singleflight-different-cache-dir",
	}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	storageInfo := metadata.StorageInfo.(*common.OCIStorageInfo)
	layer := &blockingCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}

	storage1 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: t.TempDir(),
	}
	storage2 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: t.TempDir(),
	}

	errs := make(chan error, 2)
	go func() {
		_, _, err := storage1.ensureLayerCached(context.Background(), digest.String())
		errs <- err
	}()

	<-layer.started
	go func() {
		_, _, err := storage2.ensureLayerCached(context.Background(), digest.String())
		errs <- err
	}()

	close(layer.release)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.Equal(t, 2, layer.Calls(), "separate cache directories must materialize separate local files")

	for _, dir := range []string{storage1.diskCacheDir, storage2.diskCacheDir} {
		got, err := os.ReadFile(filepath.Join(dir, decompressedHash))
		require.NoError(t, err)
		require.Equal(t, testData, got)
	}
}

func TestOCIStorage_ConcurrentDirectDecompressionUsesUniqueTempFiles(t *testing.T) {
	testData := []byte("shared layer materialization from separate worker processes")
	compressedData := createGzipData(t, testData)

	digest := v1.Hash{
		Algorithm: "sha256",
		Hex:       "direct-concurrent-layer",
	}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	diskCacheDir := t.TempDir()
	diskPath := filepath.Join(diskCacheDir, decompressedHash)

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	storageInfo := metadata.StorageInfo.(*common.OCIStorageInfo)
	layer := &barrierCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   make(chan struct{}, 2),
		release:   make(chan struct{}),
	}

	storage1 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}
	storage2 := &OCIClipStorage{
		metadata:     metadata,
		storageInfo:  storageInfo,
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
	}

	errs := make(chan error, 2)
	go func() { errs <- storage1.decompressAndCacheLayer(digest.String(), diskPath) }()
	go func() { errs <- storage2.decompressAndCacheLayer(digest.String(), diskPath) }()

	<-layer.started
	<-layer.started
	close(layer.release)

	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.Equal(t, 2, layer.Calls())

	got, err := os.ReadFile(diskPath)
	require.NoError(t, err)
	require.Equal(t, testData, got)
}

func TestOCIStorage_FullContentCacheStreamAvoidsRegistry(t *testing.T) {
	testData := []byte("verified decompressed layer from shared cache")
	digest := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])

	cache := &streamingMockCache{mockCache: mockCache{store: map[string][]byte{
		decompressedHash: testData,
	}}}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): &mockLayer{digest: digest, fetchError: errors.New("registry should not be read")}},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}

	_, layerPath, err := storage.ensureLayerCached(context.Background(), digest.String())
	require.NoError(t, err)
	got, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, got)
	require.Equal(t, 1, cache.streamCalls)
}

func TestOCIStorage_InvalidContentCacheStreamFallsBackToRegistry(t *testing.T) {
	testData := []byte("registry layer after corrupt shared cache stream")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("b", 64)}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])

	cache := &streamingMockCache{mockCache: mockCache{store: map[string][]byte{
		decompressedHash: []byte("corrupt"),
	}}}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)
	layer := &barrierCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   started,
		release:   release,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}

	_, layerPath, err := storage.ensureLayerCached(context.Background(), digest.String())
	require.NoError(t, err)
	got, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, got)
	require.Equal(t, 1, cache.streamCalls)
	require.Equal(t, 1, layer.Calls())
}

func TestOCIStorage_NilDecompressedContentCacheStreamFallsBackToRegistry(t *testing.T) {
	testData := []byte("registry layer after nil decompressed cache stream")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("c", 64)}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	cache := &streamingMockCache{
		mockCache:     *newMockCache(),
		nilStreamHash: decompressedHash,
		nilStreamSize: int64(len(testData)),
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)
	layer := &barrierCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   started,
		release:   release,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}

	_, layerPath, err := storage.ensureLayerCached(context.Background(), digest.String())
	require.NoError(t, err)
	require.Equal(t, 1, layer.Calls())
	materialized, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, materialized)
}

func TestOCIStorage_CompressedContentCacheAvoidsRegistry(t *testing.T) {
	testData := bytes.Repeat([]byte("compressed-cache-primary-source-"), 4096)
	compressedData := createGzipData(t, testData)
	compressedSum := sha256.Sum256(compressedData)
	digest := "sha256:" + hex.EncodeToString(compressedSum[:])
	decompressedSum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(decompressedSum[:])

	cache := &scriptedCompressedStreamCache{
		mockCache:    *newMockCache(),
		key:          strings.TrimPrefix(digest, "sha256:"),
		data:         compressedData,
		reportedSize: int64(len(compressedData)),
		exists:       true,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest: decompressedHash},
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{
			Digest: digest,
			Size:   int64(len(compressedData)),
		}}},
	}}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest: &mockLayer{fetchError: errors.New("registry must not be read")}},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}
	layerPath := storage.getDecompressedCachePath(decompressedHash)

	source, err := storage.decompressAndCacheLayerContext(context.Background(), digest, layerPath)
	require.NoError(t, err)
	require.Equal(t, "compressed_content_cache", source)
	require.Equal(t, 2, cache.StreamCalls(), "one decompressed miss and one compressed hit")
	materialized, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, materialized)
}

func TestOCIStorage_CorruptCompressedContentCacheFallsBackWithoutPublishing(t *testing.T) {
	testData := bytes.Repeat([]byte("compressed-cache-corruption-"), 4096)
	compressedData := createGzipData(t, testData)
	compressedSum := sha256.Sum256(compressedData)
	digest := "sha256:" + hex.EncodeToString(compressedSum[:])
	decompressedSum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(decompressedSum[:])
	corruptData := append([]byte(nil), compressedData...)
	require.Greater(t, len(corruptData), 9)
	corruptData[9] ^= 0x01 // Valid gzip with identical output but a different compressed digest.

	cache := &scriptedCompressedStreamCache{
		mockCache:    *newMockCache(),
		key:          strings.TrimPrefix(digest, "sha256:"),
		data:         corruptData,
		reportedSize: int64(len(compressedData)),
		exists:       true,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest: decompressedHash},
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{
			Digest: digest,
			Size:   int64(len(compressedData)),
		}}},
	}}
	layer := &blockingCountingLayer{
		mockLayer: &mockLayer{compressedData: compressedData},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest: layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}
	layerPath := storage.getDecompressedCachePath(decompressedHash)
	done := make(chan error, 1)
	go func() {
		_, _, err := storage.ensureLayerCached(context.Background(), digest)
		done <- err
	}()

	select {
	case <-layer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("registry fallback did not start")
	}
	require.NoFileExists(t, layerPath, "unverified compressed cache data must not be published")
	temps, err := filepath.Glob(layerPath + ".*.tmp")
	require.NoError(t, err)
	require.Empty(t, temps, "failed compressed cache attempt must remove its temp file")
	close(layer.release)
	require.NoError(t, <-done)
	require.Equal(t, 1, layer.Calls())
	materialized, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, materialized)
}

func TestOCIStorage_ShortCompressedContentCacheStreamFallsBack(t *testing.T) {
	testData := bytes.Repeat([]byte("short-compressed-cache-stream-"), 4096)
	compressedData := createGzipData(t, testData)
	compressedSum := sha256.Sum256(compressedData)
	digest := "sha256:" + hex.EncodeToString(compressedSum[:])
	decompressedSum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(decompressedSum[:])

	cache := &scriptedCompressedStreamCache{
		mockCache:    *newMockCache(),
		key:          strings.TrimPrefix(digest, "sha256:"),
		data:         compressedData[:len(compressedData)/2],
		reportedSize: int64(len(compressedData)),
		exists:       true,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest: decompressedHash},
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{
			Digest: digest,
			Size:   int64(len(compressedData)),
		}}},
	}}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)
	layer := &barrierCountingLayer{
		mockLayer: &mockLayer{compressedData: compressedData},
		started:   started,
		release:   release,
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest: layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}

	_, layerPath, err := storage.ensureLayerCached(context.Background(), digest)
	require.NoError(t, err)
	require.Equal(t, 1, layer.Calls())
	materialized, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, materialized)
}

func TestOCIStorage_CompressedContentCacheCancellationRemovesPartialFile(t *testing.T) {
	testData := bytes.Repeat([]byte("cancel-compressed-cache-stream-"), 4096)
	compressedData := createGzipData(t, testData)
	compressedSum := sha256.Sum256(compressedData)
	digest := "sha256:" + hex.EncodeToString(compressedSum[:])
	decompressedSum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(decompressedSum[:])
	release := make(chan struct{})
	cache := &scriptedCompressedStreamCache{
		mockCache:    *newMockCache(),
		key:          strings.TrimPrefix(digest, "sha256:"),
		data:         compressedData[:10],
		reportedSize: int64(len(compressedData)),
		exists:       true,
		started:      make(chan struct{}),
		release:      release,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest: decompressedHash},
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{
			Digest: digest,
			Size:   int64(len(compressedData)),
		}}},
	}}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest: &mockLayer{fetchError: errors.New("canceled cache read must not fall back")}},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}
	layerPath := storage.getDecompressedCachePath(decompressedHash)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := storage.decompressAndCacheLayerContext(ctx, digest, layerPath)
		done <- err
	}()
	select {
	case <-cache.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compressed cache stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled compressed cache restore did not return")
	}
	close(release)
	require.NoFileExists(t, layerPath)
	temps, err := filepath.Glob(layerPath + ".*.tmp")
	require.NoError(t, err)
	require.Empty(t, temps)
}

func TestOCIStorage_NilCompressedContentCacheStreamFallsBack(t *testing.T) {
	testData := []byte("nil compressed cache stream")
	compressedData := createGzipData(t, testData)
	compressedSum := sha256.Sum256(compressedData)
	digest := "sha256:" + hex.EncodeToString(compressedSum[:])
	decompressedSum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(decompressedSum[:])
	cache := &scriptedCompressedStreamCache{
		mockCache:    *newMockCache(),
		key:          strings.TrimPrefix(digest, "sha256:"),
		reportedSize: int64(len(compressedData)),
		exists:       true,
		nilStream:    true,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest: decompressedHash},
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{
			Digest: digest,
			Size:   int64(len(compressedData)),
		}}},
	}}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)
	layer := &barrierCountingLayer{
		mockLayer: &mockLayer{compressedData: compressedData},
		started:   started,
		release:   release,
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest: layer},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}

	_, layerPath, err := storage.ensureLayerCached(context.Background(), digest)
	require.NoError(t, err)
	require.Equal(t, 1, layer.Calls())
	materialized, err := os.ReadFile(layerPath)
	require.NoError(t, err)
	require.Equal(t, testData, materialized)
}

func TestOCIStorage_ConcurrentReadsShareCompressedContentCacheRestore(t *testing.T) {
	testData := bytes.Repeat([]byte("shared-compressed-cache-restore-"), 4096)
	compressedData := createGzipData(t, testData)
	compressedSum := sha256.Sum256(compressedData)
	digest := "sha256:" + hex.EncodeToString(compressedSum[:])
	decompressedSum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(decompressedSum[:])
	release := make(chan struct{})
	cache := &scriptedCompressedStreamCache{
		mockCache:    *newMockCache(),
		key:          strings.TrimPrefix(digest, "sha256:"),
		data:         compressedData,
		reportedSize: int64(len(compressedData)),
		exists:       true,
		started:      make(chan struct{}),
		release:      release,
	}
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		DecompressedHashByLayer: map[string]string{digest: decompressedHash},
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{
			Digest: digest,
			Size:   int64(len(compressedData)),
		}}},
	}}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest: &mockLayer{fetchError: errors.New("registry must not be read")}},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}
	node := &common.ClipNode{Remote: &common.RemoteRef{LayerDigest: digest, ULength: int64(len(testData))}}

	results := make(chan error, 2)
	go func() {
		_, err := storage.ReadFileContext(context.Background(), node, make([]byte, 128), 0)
		results <- err
	}()
	select {
	case <-cache.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compressed cache restore did not start")
	}
	go func() {
		_, err := storage.ReadFileContext(context.Background(), node, make([]byte, 128), 128)
		results <- err
	}()
	require.Never(t, func() bool { return cache.StreamCalls() > 2 }, 50*time.Millisecond, 5*time.Millisecond)
	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, 2, cache.StreamCalls(), "concurrent reads must share one compressed cache restore")
}

func TestOCIStorage_PrepareLayersUsesBoundedConcurrency(t *testing.T) {
	const (
		layerCount  = 6
		concurrency = 3
	)

	started := make(chan struct{}, layerCount)
	release := make(chan struct{})
	layers := make([]string, 0, layerCount)
	layerCache := make(map[string]v1.Layer, layerCount)
	hashes := make(map[string]string, layerCount)
	var expectedBytes int64
	for i := 0; i < layerCount; i++ {
		data := []byte(fmt.Sprintf("prepared layer %d", i))
		digest := v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%064x", i+1)}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		layers = append(layers, digest.String())
		hashes[digest.String()] = hash
		layerCache[digest.String()] = &barrierCountingLayer{
			mockLayer: &mockLayer{digest: digest, compressedData: createGzipData(t, data)},
			started:   started,
			release:   release,
		}
		expectedBytes += int64(len(data))
	}

	storageInfo := &common.OCIStorageInfo{Layers: layers, DecompressedHashByLayer: hashes}
	storage := &OCIClipStorage{
		metadata:     &common.ClipArchiveMetadata{StorageInfo: storageInfo},
		storageInfo:  storageInfo,
		layerCache:   layerCache,
		diskCacheDir: t.TempDir(),
	}

	var progressMu sync.Mutex
	var progress []PrepareProgress
	done := make(chan error, 1)
	go func() {
		done <- storage.Prepare(context.Background(), PrepareOptions{
			Concurrency: concurrency,
			Progress: func(update PrepareProgress) {
				progressMu.Lock()
				progress = append(progress, update)
				progressMu.Unlock()
			},
		})
	}()

	for i := 0; i < concurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("prepare did not fill its concurrency window")
		}
	}
	select {
	case <-started:
		t.Fatal("prepare exceeded its concurrency limit")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-done)

	progressMu.Lock()
	require.NotEmpty(t, progress)
	final := progress[len(progress)-1]
	progressMu.Unlock()
	require.Equal(t, layerCount, final.Completed)
	require.Equal(t, layerCount, final.Total)
	require.Equal(t, expectedBytes, final.Bytes)
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

type localPathTrackingCache struct {
	mockCache
	localPathCalls int
	localPath      string
	routingKey     string
}

func (c *localPathTrackingCache) StoreContentFromLocalPath(path string, hash string, opts struct{ RoutingKey string }) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.localPathCalls++
	c.localPath = path
	c.routingKey = opts.RoutingKey

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	c.store[hash] = data
	return hash, nil
}

func TestStoreDecompressedInRemoteCachePrefersLocalPathStore(t *testing.T) {
	testData := []byte("decompressed layer bytes")
	tmpFile := filepath.Join(t.TempDir(), "layer.dat")
	require.NoError(t, os.WriteFile(tmpFile, testData, 0644))

	cache := &localPathTrackingCache{
		mockCache: mockCache{
			store: make(map[string][]byte),
		},
	}
	storage := &OCIClipStorage{contentCache: cache}

	const digest = "sha256:localpath123"
	require.NoError(t, storage.storeDecompressedInRemoteCache(digest, tmpFile))

	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Equal(t, 1, cache.localPathCalls)
	require.Equal(t, tmpFile, cache.localPath)
	require.Equal(t, digest, cache.routingKey)
	require.Equal(t, 0, cache.setCalls, "local path store should avoid streaming StoreContent fallback")
	require.Equal(t, testData, cache.store[digest])
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

	require.NoError(t, storage.storeDecompressedInRemoteCache(digest, tmpFile))

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

	require.NoError(t, storage.storeDecompressedInRemoteCache(digest, tmpFile))

	// Verify
	cache.mu.Lock()
	defer cache.mu.Unlock()

	assert.Equal(t, 1, len(cache.chunksReceived), "small file should be single chunk")
	assert.Equal(t, len(testData), cache.chunksReceived[0], "chunk size should match file size")

	// Verify content was stored with the digest as key (test calls storeDecompressedInRemoteCache with digest directly)
	assert.Equal(t, testData, cache.store[digest], "cached content should match original")
}

type blockingStoreCache struct {
	mockCache
	started chan struct{}
	release chan struct{}
}

func (c *blockingStoreCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	close(c.started)
	<-c.release
	return c.mockCache.StoreContent(chunks, hash, opts)
}

func TestDecompressAndCacheLayerDoesNotWaitForContentCacheStore(t *testing.T) {
	testData := []byte("durable decompressed layer")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "syncstore123"}

	hasher := sha256.New()
	hasher.Write(testData)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

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
	cache := &blockingStoreCache{
		mockCache: mockCache{store: make(map[string][]byte)},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	storage := &OCIClipStorage{
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): &mockLayer{digest: digest, compressedData: compressedData}},
		diskCacheDir:          t.TempDir(),
		contentCache:          cache,
		contentCacheAvailable: true,
	}

	done := make(chan error, 1)
	go func() {
		done <- storage.decompressAndCacheLayer(digest.String(), storage.getDecompressedCachePath(decompressedHash))
	}()

	select {
	case <-cache.started:
	case <-time.After(time.Second):
		t.Fatal("content cache store did not start")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("decompressAndCacheLayer waited for content cache store")
	}

	close(cache.release)
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return bytes.Equal(testData, cache.store[decompressedHash])
	}, time.Second, 10*time.Millisecond)
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
		metadata:              metadata,
		storageInfo:           metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:            map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:          diskCacheDir,
		contentCache:          cache,
		contentCacheAvailable: true,
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

	t.Logf("? SUCCESS: %d reads completed - layer decompressed once and cached to disk!", numReads)
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
		metadata:     metadata,
		storageInfo:  metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:   map[string]v1.Layer{digest.String(): layer},
		diskCacheDir: diskCacheDir,
		contentCache: nil, // No remote cache for benchmark
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
		metadata:     metadata1,
		storageInfo:  metadata1.StorageInfo.(*common.OCIStorageInfo),
		layerCache:   map[string]v1.Layer{sharedDigest.String(): image1Layer},
		diskCacheDir: diskCacheDir,
		contentCache: nil, // No remote cache for this test
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
		metadata:     metadata2,
		storageInfo:  metadata2.StorageInfo.(*common.OCIStorageInfo),
		layerCache:   map[string]v1.Layer{sharedDigest.String(): image2Layer},
		diskCacheDir: diskCacheDir, // SAME disk cache directory!
		contentCache: nil,
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

	t.Logf("? SUCCESS: Image 2 reused cached layer from Image 1!")
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
		metadata:       metadata,
		storageInfo:    metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:     map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:   t.TempDir(),
		contentCache:   nil,
		useCheckpoints: true, // Enable checkpoint-based reading
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

	t.Log("? Checkpoint-based reading test passed!")
}

func TestOffsetOnlyCheckpointReadMaterializesVerifiedLayer(t *testing.T) {
	testData := bytes.Repeat([]byte("offset-only-checkpoint-"), 1024)
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "offset_only_materialize"}

	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: []common.GzipCheckpoint{{COff: 1234, UOff: 4096}},
				},
			},
			DecompressedHashByLayer: map[string]string{
				digest.String(): decompressedHash,
			},
		},
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)
	layer := &barrierCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
		started:   started,
		release:   release,
	}
	storage := &OCIClipStorage{
		metadata:       metadata,
		storageInfo:    metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:     map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:   t.TempDir(),
		useCheckpoints: true,
	}

	node := &common.ClipNode{
		Remote: &common.RemoteRef{
			LayerDigest: digest.String(),
			UOffset:     0,
			ULength:     int64(len(testData)),
		},
	}
	dest := make([]byte, 128)
	n, err := storage.ReadFile(node, dest, 64)
	require.NoError(t, err)
	require.Equal(t, len(dest), n)
	require.Equal(t, testData[64:64+len(dest)], dest)
	require.Equal(t, 1, layer.Calls())

	materialized, err := os.ReadFile(storage.getDecompressedCachePath(decompressedHash))
	require.NoError(t, err)
	require.Equal(t, testData, materialized)
}

func TestOffsetOnlyCheckpointConcurrentFirstMissCoalesces(t *testing.T) {
	testData := bytes.Repeat([]byte("coalesced-offset-only-read-"), 4096)
	digest := v1.Hash{Algorithm: "sha256", Hex: "offset_only_coalesced"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		GzipIdxByLayer: map[string]*common.GzipIndex{
			digest.String(): {LayerDigest: digest.String(), Checkpoints: []common.GzipCheckpoint{{COff: 42, UOff: 1024}}},
		},
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	layer := &blockingCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: createGzipData(t, testData)},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	storage := &OCIClipStorage{
		metadata:       metadata,
		storageInfo:    metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:     map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:   t.TempDir(),
		useCheckpoints: true,
	}
	node := &common.ClipNode{Remote: &common.RemoteRef{
		LayerDigest: digest.String(),
		ULength:     int64(len(testData)),
	}}

	type readResult struct {
		data []byte
		err  error
	}
	read := func(offset int64) <-chan readResult {
		done := make(chan readResult, 1)
		go func() {
			data := make([]byte, 256)
			_, err := storage.ReadFileContext(context.Background(), node, data, offset)
			done <- readResult{data: data, err: err}
		}()
		return done
	}

	owner := read(64)
	select {
	case <-layer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first read did not start layer materialization")
	}
	waiter := read(2048)
	require.Never(t, func() bool { return layer.Calls() > 1 }, 50*time.Millisecond, 5*time.Millisecond)
	close(layer.release)

	ownerResult := <-owner
	require.NoError(t, ownerResult.err)
	require.Equal(t, testData[64:64+256], ownerResult.data)
	waiterResult := <-waiter
	require.NoError(t, waiterResult.err)
	require.Equal(t, testData[2048:2048+256], waiterResult.data)
	require.Equal(t, 1, layer.Calls(), "concurrent first reads must share one full materialization")
}

func TestOffsetOnlyCheckpointFirstMissWaiterHonorsCancellation(t *testing.T) {
	testData := bytes.Repeat([]byte("cancel-offset-only-waiter-"), 4096)
	digest := v1.Hash{Algorithm: "sha256", Hex: "offset_only_canceled_waiter"}
	sum := sha256.Sum256(testData)
	decompressedHash := hex.EncodeToString(sum[:])
	metadata := &common.ClipArchiveMetadata{StorageInfo: &common.OCIStorageInfo{
		GzipIdxByLayer: map[string]*common.GzipIndex{
			digest.String(): {LayerDigest: digest.String(), Checkpoints: []common.GzipCheckpoint{{COff: 42, UOff: 1024}}},
		},
		DecompressedHashByLayer: map[string]string{digest.String(): decompressedHash},
	}}
	layer := &blockingCountingLayer{
		mockLayer: &mockLayer{digest: digest, compressedData: createGzipData(t, testData)},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	storage := &OCIClipStorage{
		metadata:       metadata,
		storageInfo:    metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:     map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:   t.TempDir(),
		useCheckpoints: true,
	}
	node := &common.ClipNode{Remote: &common.RemoteRef{
		LayerDigest: digest.String(),
		ULength:     int64(len(testData)),
	}}

	ownerDone := make(chan error, 1)
	go func() {
		_, err := storage.ReadFileContext(context.Background(), node, make([]byte, 128), 0)
		ownerDone <- err
	}()
	select {
	case <-layer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first read did not start layer materialization")
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := storage.ReadFileContext(waiterCtx, node, make([]byte, 128), 128)
		waiterDone <- err
	}()
	cancelWaiter()
	select {
	case err := <-waiterDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled read remained blocked on layer materialization")
	}
	select {
	case err := <-ownerDone:
		t.Fatalf("owner unexpectedly completed before release: %v", err)
	default:
	}
	require.Equal(t, 1, layer.Calls())

	close(layer.release)
	select {
	case err := <-ownerDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not complete after release")
	}
}

func TestCheckpointReadShortReadFails(t *testing.T) {
	testData := []byte("short checkpoint data")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "checkpoint_short_read"}

	metadata := &common.ClipArchiveMetadata{
		StorageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: []common.GzipCheckpoint{{COff: 0, UOff: 0}},
				},
			},
		},
	}
	storage := &OCIClipStorage{
		metadata:    metadata,
		storageInfo: metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:  map[string]v1.Layer{digest.String(): &mockLayer{digest: digest, compressedData: compressedData}},
	}

	dest := make([]byte, len(testData)+1)
	n, err := storage.readWithCheckpoint(context.Background(), digest.String(), 0, dest)
	require.Error(t, err)
	require.Equal(t, len(testData), n)
	require.Contains(t, err.Error(), "unexpected EOF")
}

func TestCheckpointReadIgnoresOffsetOnlyRestartPoint(t *testing.T) {
	prefix := bytes.Repeat([]byte("x"), 3*1024*1024)
	target := []byte("exact partial read")
	testData := append(append([]byte{}, prefix...), target...)
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "offset_only_checkpoint"}

	storage := &OCIClipStorage{
		storageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: []common.GzipCheckpoint{{COff: 12345, UOff: int64(len(prefix))}},
				},
			},
		},
		layerCache:     map[string]v1.Layer{digest.String(): &mockLayer{digest: digest, compressedData: compressedData}},
		useCheckpoints: true,
	}

	dest := make([]byte, len(target))
	n, err := storage.readWithCheckpoint(context.Background(), digest.String(), int64(len(prefix)), dest)
	require.NoError(t, err)
	require.Equal(t, len(target), n)
	require.Equal(t, target, dest)
}

func TestCheckpointReadIgnoresMalformedStreamStartCheckpoint(t *testing.T) {
	prefix := bytes.Repeat([]byte("x"), 1024)
	target := []byte("target")
	testData := append(append([]byte{}, prefix...), target...)
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "malformed_stream_start_checkpoint"}

	storage := &OCIClipStorage{
		storageInfo: &common.OCIStorageInfo{
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: []common.GzipCheckpoint{{COff: 0, UOff: int64(len(prefix))}},
				},
			},
		},
		layerCache:     map[string]v1.Layer{digest.String(): &mockLayer{digest: digest, compressedData: compressedData}},
		useCheckpoints: true,
	}

	dest := make([]byte, len(target))
	n, err := storage.readWithCheckpoint(context.Background(), digest.String(), int64(len(prefix)), dest)
	require.NoError(t, err)
	require.Equal(t, len(target), n)
	require.Equal(t, target, dest)
}

func TestCheckpointReadCachesFetchedLayerWithDetachedContext(t *testing.T) {
	testData := []byte("checkpoint read should not poison layer cache")
	compressedData := createGzipData(t, testData)
	digest := v1.Hash{Algorithm: "sha256", Hex: "detached_context_layer"}

	previousFetch := fetchOCILayerByDigest
	defer func() { fetchOCILayerByDigest = previousFetch }()

	var fetchCtx context.Context
	fetchOCILayerByDigest = func(ctx context.Context, storage *OCIClipStorage, gotDigest string) (v1.Layer, error) {
		require.Equal(t, digest.String(), gotDigest)
		fetchCtx = ctx
		return &contextBoundLayer{
			mockLayer: &mockLayer{digest: digest, compressedData: compressedData},
			ctx:       ctx,
		}, nil
	}

	storage := &OCIClipStorage{
		storageInfo: &common.OCIStorageInfo{
			RegistryURL: "registry.example.com",
			Repository:  "team/image",
			GzipIdxByLayer: map[string]*common.GzipIndex{
				digest.String(): {
					LayerDigest: digest.String(),
					Checkpoints: []common.GzipCheckpoint{{COff: 0, UOff: 0}},
				},
			},
		},
		layerCache:     map[string]v1.Layer{},
		useCheckpoints: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	dest := make([]byte, len(testData))
	n, err := storage.readWithCheckpoint(ctx, digest.String(), 0, dest)
	require.NoError(t, err)
	require.Equal(t, len(testData), n)
	require.Equal(t, testData, dest)

	cancel()
	require.NotNil(t, fetchCtx)
	require.NoError(t, fetchCtx.Err(), "cached layer must not retain the foreground read cancellation")

	layer := storage.layerCache[digest.String()]
	require.NotNil(t, layer)
	compressed, err := layer.Compressed()
	require.NoError(t, err)
	require.NoError(t, compressed.Close())
}

func TestCachedLayerByDigestHonorsAlreadyCanceledContext(t *testing.T) {
	digest := v1.Hash{Algorithm: "sha256", Hex: "canceled_context_layer"}
	previousFetch := fetchOCILayerByDigest
	defer func() { fetchOCILayerByDigest = previousFetch }()

	fetchCalled := false
	fetchOCILayerByDigest = func(ctx context.Context, storage *OCIClipStorage, gotDigest string) (v1.Layer, error) {
		fetchCalled = true
		return &mockLayer{digest: digest, compressedData: createGzipData(t, []byte("data"))}, nil
	}

	storage := &OCIClipStorage{
		storageInfo: &common.OCIStorageInfo{
			RegistryURL: "registry.example.com",
			Repository:  "team/image",
		},
		layerCache: map[string]v1.Layer{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := storage.cachedLayerByDigest(ctx, digest.String())
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, fetchCalled)
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
		metadata:       metadata,
		storageInfo:    metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:     map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:   t.TempDir(),
		contentCache:   nil,
		useCheckpoints: true, // Enabled but no checkpoints
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

	t.Log("? Checkpoint fallback test passed!")
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
		metadata:       metadata,
		storageInfo:    metadata.StorageInfo.(*common.OCIStorageInfo),
		layerCache:     map[string]v1.Layer{digest.String(): layer},
		diskCacheDir:   t.TempDir(),
		contentCache:   nil,
		useCheckpoints: false, // Checkpoints DISABLED (backward compatibility)
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

	t.Log("? Backward compatibility test passed!")
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
		name         string
		wantUOffset  int64
		expectedCOff int64
		expectedUOff int64
		description  string
	}{
		{"At first checkpoint", 0, 100, 0, "should use first checkpoint"},
		{"Exactly at checkpoint", 2 * 1024 * 1024, 200, 2 * 1024 * 1024, "should use exact checkpoint"},
		{"Between checkpoints", 3 * 1024 * 1024, 200, 2 * 1024 * 1024, "should use previous checkpoint"},
		{"After last checkpoint", 10 * 1024 * 1024, 400, 6 * 1024 * 1024, "should use last checkpoint"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cOff, uOff := common.NearestCheckpoint(checkpoints, tc.wantUOffset)
			assert.Equal(t, tc.expectedCOff, cOff, "compressed offset should match")
			assert.Equal(t, tc.expectedUOff, uOff, "uncompressed offset should match")
			t.Logf("%s: wantU=%d -> cOff=%d, uOff=%d", tc.description, tc.wantUOffset, cOff, uOff)
		})
	}
}

func TestNearestCheckpointBeforeFirstReturnsStreamStart(t *testing.T) {
	checkpoints := []common.GzipCheckpoint{
		{COff: 200, UOff: 2 * 1024 * 1024},
		{COff: 300, UOff: 4 * 1024 * 1024},
	}

	cOff, uOff := common.NearestCheckpoint(checkpoints, 1024)
	assert.Equal(t, int64(0), cOff, "compressed offset should fall back to stream start")
	assert.Equal(t, int64(0), uOff, "uncompressed offset should fall back to stream start")
}

// TestCheckpointEmptyList tests NearestCheckpoint with empty checkpoint list
func TestCheckpointEmptyList(t *testing.T) {
	cOff, uOff := common.NearestCheckpoint([]common.GzipCheckpoint{}, 1000)
	assert.Equal(t, int64(0), cOff, "should return 0 for empty checkpoint list")
	assert.Equal(t, int64(0), uOff, "should return 0 for empty checkpoint list")
}

func TestNoteContentCacheServedCrossesThresholdOnce(t *testing.T) {
	s := &OCIClipStorage{}
	half := int64(contentCacheWarmThreshold / 2)
	require.False(t, s.noteContentCacheServed("a", half), "below threshold")
	require.True(t, s.noteContentCacheServed("a", half), "crossing threshold schedules the warm")
	require.False(t, s.noteContentCacheServed("a", half), "only once per layer")
	require.False(t, s.noteContentCacheServed("b", half), "layers are counted separately")
}
