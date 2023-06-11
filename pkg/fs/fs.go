package fs

import (
	"fmt"
	"log"

	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type FileSystem struct {
	s    storage.ClipStorageInterface
	root *Dir
}

func NewFileSystem(s storage.ClipStorageInterface) *FileSystem {
	fsys := &FileSystem{
		s: s,
	}

	metadata := s.Metadata()
	rootNode := metadata.Get("/log")

	log.Println("root node: ", rootNode)
	fsys.root = &Dir{
		fsys: fsys,
		attr: rootNode.Attr,
	}

	return fsys
}

func (fsys *FileSystem) Root() (fs.InodeEmbedder, error) {
	if fsys.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return fsys.root, nil
}
