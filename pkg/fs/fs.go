package fs

import (
	"fmt"
	"log"

	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type ClipFileSystem struct {
	s    storage.ClipStorageInterface
	root *Dir
}

func NewFileSystem(s storage.ClipStorageInterface) *ClipFileSystem {
	cfs := &ClipFileSystem{
		s: s,
	}

	metadata := s.Metadata()
	rootNode := metadata.Get("/log")

	log.Println("root node: ", rootNode)
	cfs.root = &Dir{
		cfs:  cfs,
		attr: rootNode.Attr,
	}

	return cfs
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}
