package archive

import (
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tidwall/btree"
)

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
	DataPos  int64 // Position of the nodes data in the final binary
	DataLen  int64 // Length of the nodes data
}

type ClipArchiveMetadata struct {
	Header ClipArchiveHeader
	Index  *btree.BTree
}

func (m *ClipArchiveMetadata) Insert(node *ClipNode) {
	m.Index.Set(node)
}

func (m *ClipArchiveMetadata) Get(path string) *ClipNode {
	item := m.Index.Get(&ClipNode{Path: path})
	if item == nil {
		return nil
	}
	return item.(*ClipNode)
}

func (m *ClipArchiveMetadata) ListDirectory(path string) []*ClipNode {
	var entries []*ClipNode

	// Append null character to the path -- if we don't do this we could miss some child nodes.
	// It works because \x00 is lower lexagraphically than any other character
	pivot := &ClipNode{Path: path + "\x00"}
	m.Index.Ascend(pivot, func(a interface{}) bool {
		node := a.(*ClipNode)

		// Remove the prefix and check if there are any "/" left
		relativePath := strings.TrimPrefix(node.Path, path)
		if strings.Contains(relativePath, "/") {
			// This node is not an immediate child, continue on
			return true
		}

		// Node is an immediate child, so we append it to entries
		if relativePath != "" {
			entries = append(entries, node)
		}

		return true
	})

	return entries
}
