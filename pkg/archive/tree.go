package archive

import (
	"encoding/gob"
	"fmt"
	"os"

	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&FsNode{})
}

type NodeType string

const (
	DirNode     NodeType = "dir"
	FileNode    NodeType = "file"
	SymLinkNode NodeType = "symlink"
)

type FsNode struct {
	NodeType NodeType
	Path     string
	Size     int64  // For FileNode, could be 0 for DirNode and SymLinkNode
	Target   string // For SymLinkNode, empty for DirNode and FileNode
}

type FileSystem struct {
	tree *btree.BTree
}

func NewFileSystem() *FileSystem {
	compare := func(a, b interface{}) bool {
		return a.(*FsNode).Path < b.(*FsNode).Path
	}

	return &FileSystem{tree: btree.New(compare)}
}

func (fs *FileSystem) Insert(node *FsNode) {
	fs.tree.Set(node)
}

func (fs *FileSystem) DumpToFile(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	enc := gob.NewEncoder(file)
	return enc.Encode(fs.tree)
}

func (fs *FileSystem) LoadFromFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	return dec.Decode(&fs.tree)
}

func (fs *FileSystem) PrintNodes() {
	fs.tree.Ascend(fs.tree.Min(), func(a interface{}) bool {
		node := a.(*FsNode)
		fmt.Printf("Path: %s, NodeType: %s\n", node.Path, node.NodeType)
		return true
	})
}
