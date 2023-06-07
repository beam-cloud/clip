package archive

import (
	"encoding/gob"
	"fmt"
	"os"

	"bazil.org/fuse"
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
	Attr     fuse.Attr
	Target   string
}

type ClipFile struct {
	Index *btree.BTree
}

func NewClipFile() *ClipFile {
	compare := func(a, b interface{}) bool {
		return a.(*ClipFSNode).Path < b.(*ClipFSNode).Path
	}

	return &ClipFile{Index: btree.New(compare)}
}

func (cfs *ClipFile) Insert(node *ClipFSNode) {
	cfs.Index.Set(node)
}

func (cfs *ClipFile) Get(path string) *ClipFSNode {
	item := cfs.Index.Get(&ClipFSNode{Path: path})
	if item == nil {
		return nil
	}
	return item.(*ClipFSNode)
}

func (cfs *ClipFile) Dump(filename string) error {
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

func (cfs *ClipFile) Load(filename string) error {
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

func (cfs *ClipFile) PrintNodes() {
	cfs.Index.Ascend(cfs.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipFSNode)
		count += 1
		fmt.Printf("Path: %s, NodeType: %s, count: %d\n", node.Path, node.NodeType, count)
		return true
	})
}
