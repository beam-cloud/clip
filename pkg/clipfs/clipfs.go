package clipfs

import (
	"fmt"

	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type ClipFileSystem struct {
	s       storage.ClipStorageInterface
	root    *FSNode
	verbose bool
}

func NewFileSystem(s storage.ClipStorageInterface, verbose bool) *ClipFileSystem {
	cfs := &ClipFileSystem{
		s:       s,
		verbose: verbose,
	}

	metadata := s.Metadata()
	rootNode := metadata.Get("/")
	cfs.root = &FSNode{
		filesystem: cfs,
		attr:       rootNode.Attr,
		clipNode:   rootNode,
	}

	return cfs
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}
