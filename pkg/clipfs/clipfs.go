package clipfs

import (
	"fmt"
	"sync"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type ClipFileSystemOpts struct {
	Verbose      bool
	ContentCache ContentCache
}

type ClipFileSystem struct {
	s            storage.ClipStorageInterface
	root         *FSNode
	lookupCache  map[string]*lookupCacheEntry
	contentCache ContentCache
	cacheMutex   sync.RWMutex
	verbose      bool
}

type lookupCacheEntry struct {
	inode *fs.Inode
	attr  fuse.Attr
}

type ContentCache interface {
	Get(hash string, offset int64, length int64) ([]byte, error)
	Store(content []byte) (string, error)
}

func NewFileSystem(s storage.ClipStorageInterface, opts ClipFileSystemOpts) (*ClipFileSystem, error) {
	cfs := &ClipFileSystem{
		s:            s,
		verbose:      opts.Verbose,
		lookupCache:  make(map[string]*lookupCacheEntry),
		contentCache: opts.ContentCache,
	}

	metadata := s.Metadata()
	rootNode := metadata.Get("/")
	if rootNode == nil {
		return nil, common.ErrMissingArchiveRoot
	}

	cfs.root = &FSNode{
		filesystem: cfs,
		attr:       rootNode.Attr,
		clipNode:   rootNode,
	}

	return cfs, nil
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}
