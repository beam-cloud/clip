package clipfs

import (
	"fmt"
	"sync"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type ClipFileSystem struct {
	s           storage.ClipStorageInterface
	root        *FSNode
	lookupCache map[string]*cacheEntry
	cacheMutex  sync.RWMutex
	verbose     bool
}

type cacheEntry struct {
	inode *fs.Inode
	attr  fuse.Attr
}

func NewFileSystem(s storage.ClipStorageInterface, verbose bool) (*ClipFileSystem, error) {
	cfs := &ClipFileSystem{
		s:           s,
		verbose:     verbose,
		lookupCache: make(map[string]*cacheEntry),
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
