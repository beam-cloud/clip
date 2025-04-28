package clip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/tidwall/btree"
)

// Mock implementation of ContentCache for testing
type mockContentCache struct {
	mu          sync.Mutex
	store       map[string][]byte
	storeDone   map[string]chan struct{} // Signals when StoreContent completes
	storeDoneMu sync.Mutex               // Protects storeDone map

	// Tracking fields for assertions
	getCalled bool
	getError  error
}

func newMockContentCache() *mockContentCache {
	return &mockContentCache{
		store:     make(map[string][]byte),
		storeDone: make(map[string]chan struct{}),
	}
}

func (m *mockContentCache) resetTrackingFields() {
	m.mu.Lock()
	m.getCalled = false
	m.getError = nil
	m.mu.Unlock()
}

// GetContent now checks the internal store first
func (m *mockContentCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalled = true // Track that GetContent was called

	data, found := m.store[hash]
	if found {
		// Cache Hit
		m.getError = nil
		end := offset + length
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		if offset >= int64(len(data)) {
			return []byte{}, nil // Read beyond EOF
		}
		return data[offset:end], nil
	}

	// Cache Miss
	m.getError = errors.New("cache miss")
	return nil, m.getError
}

// StoreContent now signals completion
func (m *mockContentCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	data := []byte{}
	for chunk := range chunks {
		data = append(data, chunk...)
	}

	m.mu.Lock()
	m.store[hash] = data
	m.mu.Unlock()

	// Signal completion
	m.storeDoneMu.Lock()
	ch, exists := m.storeDone[hash]
	if !exists {
		ch = make(chan struct{})
		m.storeDone[hash] = ch
	}
	close(ch) // Close channel to signal completion
	m.storeDoneMu.Unlock()

	return hash, nil
}

// WaitForStore waits until StoreContent has been called for the given hash
func (m *mockContentCache) WaitForStore(hash string, timeout time.Duration) error {
	m.storeDoneMu.Lock()
	ch, exists := m.storeDone[hash]
	if !exists {
		ch = make(chan struct{})
		m.storeDone[hash] = ch
	}
	m.storeDoneMu.Unlock()

	select {
	case <-ch:
		return nil // Store completed
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for store on hash %s", hash)
	}
}

// Add tracking for storage ReadFile calls
type mockS3Storage struct {
	storage.ClipStorageInterface
	readFileCalled bool
	readFileError  error
	mu             sync.Mutex
}

func (m *mockS3Storage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	m.mu.Lock()
	m.readFileCalled = true
	m.mu.Unlock()

	if m.readFileError != nil {
		return 0, m.readFileError
	}
	return m.ClipStorageInterface.ReadFile(node, dest, offset)
}

func (m *mockS3Storage) WasReadFileCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readFileCalled
}

func (m *mockS3Storage) resetTrackingFields() {
	m.mu.Lock()
	m.readFileCalled = false
	m.mu.Unlock()
}

func Test_FSNodeLookupAndRead(t *testing.T) {
	ctx := context.Background()

	req := tc.ContainerRequest{
		Image:        "localstack/localstack:3",
		ExposedPorts: []string{"4566/tcp"},                                                  // Expose the edge service port
		WaitingFor:   wait.ForListeningPort("4566/tcp").WithStartupTimeout(2 * time.Minute), // Wait specifically for the edge service
	}
	localstackContainer, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "Failed to start localstack container")
	defer func() {
		if err := localstackContainer.Terminate(ctx); err != nil {
			t.Fatalf("Failed to terminate localstack container: %s", err)
		}
	}()

	hostPort, err := localstackContainer.MappedPort(ctx, "4566/tcp")
	require.NoError(t, err)
	hostIP, err := localstackContainer.Host(ctx)
	require.NoError(t, err)
	endpoint := "http://" + hostIP + ":" + hostPort.Port()

	accessKey := "test"
	secretKey := "test"
	region := "us-east-1"

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: endpoint, SigningRegion: region}, nil
			})),
	)
	require.NoError(t, err)

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true // Necessary for LocalStack
	})

	bucketName := "test-clip-bucket"
	_, err = s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		if !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") &&
			!strings.Contains(err.Error(), "bucket already exists") { // Add other known variations
			require.NoError(t, err, "Failed to create bucket")
		}
	}

	testFileName := "testfile.txt"
	testFileData := []byte("Hello from Clip Test!")
	testFileHashBytes := sha256.Sum256(testFileData)
	testFileHash := hex.EncodeToString(testFileHashBytes[:])
	testFilePath := "/" + testFileName
	archiveKey := "test_archive.clip"

	now := uint64(time.Now().Unix())
	rootNode := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Ino:   1,
			Mode:  fuse.S_IFDIR | 0755,
			Nlink: 2,
			Atime: now,
			Mtime: now,
			Ctime: now,
		},
	}
	testFileNode := &common.ClipNode{
		Path:        testFilePath,
		NodeType:    common.FileNode,
		ContentHash: testFileHash,
		DataLen:     int64(len(testFileData)),
		DataPos:     0,
		Attr: fuse.Attr{
			Ino:   2,
			Size:  uint64(len(testFileData)),
			Mode:  fuse.S_IFREG | 0644,
			Nlink: 1,
			Atime: now,
			Mtime: now,
			Ctime: now,
		},
	}

	// Create the BTree index for metadata
	clipNodeLess := func(a, b *common.ClipNode) bool {
		return a.Path < b.Path
	}
	index := btree.NewBTreeGOptions(clipNodeLess, btree.Options{
		NoLocks: true,
	})
	index.Set(rootNode)
	index.Set(testFileNode)

	metadata := &common.ClipArchiveMetadata{
		Header: common.ClipArchiveHeader{},
		StorageInfo: common.S3StorageInfo{
			Bucket: bucketName,
			Key:    archiveKey,
			Region: region,
		},
		Index: index,
	}

	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(archiveKey),
		Body:   bytes.NewReader(testFileData),
	})
	require.NoError(t, err, "Failed to upload test data to S3")

	mockCache := newMockContentCache()

	// No local cache file
	s3StorageOpts := storage.S3ClipStorageOpts{
		Bucket:         bucketName,
		Key:            archiveKey,
		Region:         region,
		Endpoint:       endpoint,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		CachePath:      "",
		ForcePathStyle: true,
	}
	s3Storage, err := storage.NewS3ClipStorage(metadata, s3StorageOpts)
	require.NoError(t, err, "Failed to create S3 clip storage")
	require.False(t, s3Storage.CachedLocally(), "S3 storage should not be cached locally for this test")

	mockStorage := &mockS3Storage{ClipStorageInterface: s3Storage}

	// Create ClipFileSystem instance with ContentCacheAvailable=true
	fsOpts := ClipFileSystemOpts{
		Verbose:               true,
		ContentCache:          mockCache,
		ContentCacheAvailable: true,
	}
	clipFS, err := NewFileSystem(mockStorage, fsOpts)
	require.NoError(t, err, "Failed to create ClipFileSystem")

	// Get the root InodeEmbedder
	rootInodeEmbedder, err := clipFS.Root()
	require.NoError(t, err)

	_ = fs.NewNodeFS(rootInodeEmbedder, &fs.Options{})
	rootFSNode := rootInodeEmbedder.(*FSNode)

	// Lookup on the FSNode
	lookupEntryOut := &fuse.EntryOut{}
	childInode, errno := rootFSNode.Lookup(ctx, testFileName, lookupEntryOut)
	require.Equal(t, fs.OK, errno, "Lookup failed")
	require.NotNil(t, childInode)
	testFileFSNode := childInode.Operations().(*FSNode)
	require.Equal(t, testFilePath, testFileFSNode.clipNode.Path)

	// Read on the FSNode
	readDest := make([]byte, len(testFileData)+10) // Make buffer larger than data
	readResult, readErrno := testFileFSNode.Read(ctx, nil, readDest, 0)
	require.Equal(t, fs.OK, readErrno, "Read returned an error")

	readData, status := readResult.Bytes(readDest)
	require.Equal(t, fuse.OK, status, "Read returned an error")

	// Check if data matches
	expectedReadLen := len(testFileData)
	if expectedReadLen < len(readDest) {
		expectedReadLen++ // Null terminator
	}
	assert.Len(t, readData, expectedReadLen, "Read data length mismatch")
	assert.Equal(t, testFileData, readData[:len(testFileData)], "Read data content mismatch")
	if len(readData) > len(testFileData) {
		assert.Equal(t, byte(0), readData[len(testFileData)], "Read data should be null-terminated")
	}

	// Verify the call sequence: cache then storage
	assert.True(t, mockCache.getCalled, "[First Read] mockCache.GetContent should have been called")
	assert.Error(t, mockCache.getError, "[First Read] mockCache.GetContent should have returned an error (cache miss)")
	assert.True(t, mockStorage.WasReadFileCalled(), "[First Read] mockStorage.ReadFile should have been called after cache miss")

	// === Second Read: Cache Hit Scenario ===

	// Wait for background caching to complete
	waitTimeout := 5 * time.Second // Adjust timeout as needed
	err = mockCache.WaitForStore(testFileHash, waitTimeout)
	require.NoError(t, err, "Waiting for cache store timed out")

	// Reset tracking fields on mocks before the second call
	mockCache.resetTrackingFields()
	mockStorage.resetTrackingFields()

	// Call Read again
	readResult, readErrno = testFileFSNode.Read(ctx, nil, readDest, 0)
	require.Equal(t, fs.OK, readErrno, "[Second Read] Read returned an error")

	readData, status = readResult.Bytes(readDest)
	require.Equal(t, fuse.OK, status, "[Second Read] Read Bytes returned an error")
	assert.Equal(t, testFileData, readData[:len(testFileData)], "[Second Read] Read data content mismatch")
	if len(readData) > len(testFileData) {
		assert.Equal(t, byte(0), readData[len(testFileData)], "[Second Read] Read data should be null-terminated")
	}

	// Verify that the cache was hit this time
	assert.True(t, mockCache.getCalled, "[Second Read] mockCache.GetContent should have been called")
	assert.NoError(t, mockCache.getError, "[Second Read] mockCache.GetContent should not have returned an error (cache hit)")
	assert.False(t, mockStorage.WasReadFileCalled(), "[Second Read] mockStorage.ReadFile should NOT have been called (cache hit)")
}
