package clipfs

import (
	"fmt"

	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type ClipFileSystem struct {
	s    storage.ClipStorageInterface
	root *FSNode
}

func NewFileSystem(s storage.ClipStorageInterface) *ClipFileSystem {
	cfs := &ClipFileSystem{
		s: s,
	}

	metadata := s.Metadata()
	rootNode := metadata.Get("/")
	cfs.root = &FSNode{
		cfs:      cfs,
		attr:     rootNode.Attr,
		clipNode: rootNode,
	}

	return cfs
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}
