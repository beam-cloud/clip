package archive

import (
	"encoding/gob"
	"fmt"
	"os"

	"bazil.org/fuse"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&ClipNode{})
}

type ClipNodeType string

const (
	DirNode     ClipNodeType = "dir"
	FileNode    ClipNodeType = "file"
	SymLinkNode ClipNodeType = "symlink"
)

type ClipNode struct {
	NodeType ClipNodeType
	Path     string
	Attr     fuse.Attr
	Target   string
}

type ClipArchive struct {
	Index *btree.BTree
}

func NewClipArchive() *ClipArchive {
	compare := func(a, b interface{}) bool {
		return a.(*ClipNode).Path < b.(*ClipNode).Path
	}

	return &ClipArchive{Index: btree.New(compare)}
}

func (cfs *ClipArchive) Insert(node *ClipNode) {
	cfs.Index.Set(node)
}

func (cfs *ClipArchive) Get(path string) *ClipNode {
	item := cfs.Index.Get(&ClipNode{Path: path})
	if item == nil {
		return nil
	}
	return item.(*ClipNode)
}

func (cfs *ClipArchive) Dump(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	var nodes []*ClipNode
	cfs.Index.Ascend(cfs.Index.Min(), func(a interface{}) bool {
		nodes = append(nodes, a.(*ClipNode))
		return true
	})

	enc := gob.NewEncoder(file)
	return enc.Encode(nodes)
}

func (cfs *ClipArchive) Load(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	var nodes []*ClipNode
	if err := dec.Decode(&nodes); err != nil {
		return err
	}

	for _, node := range nodes {
		cfs.Index.Set(node)
	}

	return nil
}

var count int = 0

func (cfs *ClipArchive) PrintNodes() {
	cfs.Index.Ascend(cfs.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)
		count += 1
		fmt.Printf("Path: %s, NodeType: %s, count: %d\n", node.Path, node.NodeType, count)
		return true
	})
}