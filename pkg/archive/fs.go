package archive

import (
	"encoding/gob"
	"fmt"
	"os"

	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&ClipFSNode{})
}

type NodeType string

const (
	DirNode     NodeType = "dir"
	FileNode    NodeType = "file"
	SymLinkNode NodeType = "symlink"
)

type ClipFSNode struct {
	NodeType NodeType
	Path     string
	Size     int64  // For FileNode, could be 0 for DirNode and SymLinkNode
	Target   string // For SymLinkNode, empty for DirNode and FileNode
}

type ClipFS struct {
	Index *btree.BTree
}

func NewClipFS() *ClipFS {
	compare := func(a, b interface{}) bool {
		return a.(*ClipFSNode).Path < b.(*ClipFSNode).Path
	}

	return &ClipFS{Index: btree.New(compare)}
}

func (cfs *ClipFS) Insert(node *ClipFSNode) {
	cfs.Index.Set(node)
}

func (cfs *ClipFS) DumpToFile(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	var nodes []*ClipFSNode
	cfs.Index.Ascend(cfs.Index.Min(), func(a interface{}) bool {
		nodes = append(nodes, a.(*ClipFSNode))
		return true
	})

	enc := gob.NewEncoder(file)
	return enc.Encode(nodes)
}

func (cfs *ClipFS) LoadFromFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	var nodes []*ClipFSNode
	if err := dec.Decode(&nodes); err != nil {
		return err
	}

	for _, node := range nodes {
		cfs.Index.Set(node)
	}

	return nil
}

var count int = 0

func (cfs *ClipFS) PrintNodes() {
	cfs.Index.Ascend(cfs.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipFSNode)
		count += 1
		fmt.Printf("Path: %s, NodeType: %s, count: %d\n", node.Path, node.NodeType, count)
		return true
	})
}
