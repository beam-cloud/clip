package clipfs

import (
	"fmt"
	"sync"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type ClipFileSystem struct {
	s           storage.ClipStorageInterface
	root        *FSNode
	lookupCache map[string]*fs.Inode
	cacheMutex  sync.RWMutex
	verbose     bool
}

func NewFileSystem(s storage.ClipStorageInterface, verbose bool) (*ClipFileSystem, error) {
	cfs := &ClipFileSystem{
		s:           s,
		verbose:     verbose,
		lookupCache: make(map[string]*fs.Inode),
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
