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

	var nodes []*FsNode
	fs.tree.Ascend(fs.tree.Min(), func(a interface{}) bool {
		nodes = append(nodes, a.(*FsNode))
		return true
	})

	enc := gob.NewEncoder(file)
	return enc.Encode(nodes)
}

func (fs *FileSystem) LoadFromFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	var nodes []*FsNode
	if err := dec.Decode(&nodes); err != nil {
		return err
	}

	for _, node := range nodes {
		fs.tree.Set(node)
	}

	return nil
}

var count int = 0

func (fs *FileSystem) PrintNodes() {
	fs.tree.Ascend(fs.tree.Min(), func(a interface{}) bool {
		node := a.(*FsNode)
		count += 1
		fmt.Printf("Path: %s, NodeType: %s, count: %d\n", node.Path, node.NodeType, count)
		return true
	})
}
