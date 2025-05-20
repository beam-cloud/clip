package clipv2

import (
	"fmt"
	"sync"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/beam-cloud/ristretto"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type ClipFileSystemOpts struct {
	Verbose               bool
	ContentCache          ContentCache
	ContentCacheAvailable bool
	ArchiverOptions       *ClipV2ArchiverOptions
	StorageOpts           *ClipStorageOpts
}

type ClipFileSystem struct {
	storage               storage.ClipStorageInterface
	root                  *FSNode
	lookupCache           map[string]*lookupCacheEntry
	lookupCacheMutex      sync.RWMutex
	contentCache          ContentCache
	contentCacheAvailable bool
	verbose               bool
	options               ClipFileSystemOpts
}

type lookupCacheEntry struct {
	inode *fs.Inode
	attr  fuse.Attr
}

type ContentCache interface {
	GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error)
	StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error)
	GetFileFromChunks(hash string, chunks []string, chunkBaseUrl string, chunkSize int64, startOffset int64, endOffset int64, dest []byte) (int, error)
	GetFileFromChunksWithOffset(hash string, chunks []string, chunkBaseUrl string, chunkSize int64, startOffset int64, endOffset int64, reqOffset int64, dest []byte) (int, error)
	WarmChunks(chunks []string, chunkBaseURL string) error
}

func NewFileSystem(s storage.ClipStorageInterface, chunkCache *ristretto.Cache[string, []byte], opts ClipFileSystemOpts) (*ClipFileSystem, error) {
	cfs := &ClipFileSystem{
		storage:               s,
		verbose:               opts.Verbose,
		lookupCache:           make(map[string]*lookupCacheEntry),
		contentCache:          opts.ContentCache,
		contentCacheAvailable: opts.ContentCacheAvailable,
		options:               opts,
	}

	rootNode := s.Metadata().Get("/")
	if rootNode == nil {
		return nil, common.ErrMissingArchiveRoot
	}

	cfs.root = &FSNode{
		filesystem:   cfs,
		attr:         rootNode.Attr,
		clipNode:     rootNode,
		chunkCache:   chunkCache,
		contentCache: cfs.contentCache,
		storage:      s,
	}

	return cfs, nil
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}
