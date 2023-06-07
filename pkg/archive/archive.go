package archive

import (
	"encoding/gob"
	"fmt"
	"os"

	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&ClipNode{})
}

type ClipArchive struct {
	Header ClipArchiveHeader
	Index  *btree.BTree
	Blocks []ClipArchiveBlock
}

func NewClipArchive() *ClipArchive {
	compare := func(a, b interface{}) bool {
		return a.(*ClipNode).Path < b.(*ClipNode).Path
	}
	return &ClipArchive{
		Header: ClipArchiveHeader{
			StartBytes:            ClipFileStartBytes,
			ClipFileFormatVersion: ClipFileFormatVersion,
			IndexSize:             0,
			Valid:                 false},
		Index:  btree.New(compare),
		Blocks: []ClipArchiveBlock{},
	}
}

func (ca *ClipArchive) Insert(node *ClipNode) {
	ca.Index.Set(node)
}

func (ca *ClipArchive) Get(path string) *ClipNode {
	item := ca.Index.Get(&ClipNode{Path: path})
	if item == nil {
		return nil
	}
	return item.(*ClipNode)
}

func (ca *ClipArchive) Load(filename string) error {
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
		ca.Index.Set(node)
	}

	return nil
}

func (ca *ClipArchive) dumpIndex(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	var nodes []*ClipNode
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
		nodes = append(nodes, a.(*ClipNode))
		return true
	})

	enc := gob.NewEncoder(file)
	return enc.Encode(nodes)
}

func (ca *ClipArchive) PrintNodes() {
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)
		fmt.Printf("Path: %s, NodeType: %s, count: %d\n", node.Path, node.NodeType)
		return true
	})
}
