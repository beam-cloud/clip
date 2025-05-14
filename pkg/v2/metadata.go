package clipv2

import (
	"strings"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tidwall/btree"
)

const (
	ClipV2HeaderLength            = 78
	ClipV2FileFormatVersion uint8 = 0x02
	ClipV2ChecksumLength    int64 = 8
)

type ClipV2ArchiveHeader struct {
	StartBytes            [9]byte
	ClipFileFormatVersion uint8
	ChunkPos              int64
	ChunkListLength       int64
	IndexLength           int64
	IndexPos              int64
	StorageInfoLength     int64
	StorageInfoPos        int64
	StorageInfoType       [12]byte
	ChunkSize             int64
}

type ClipV2Archive struct {
	Header      ClipV2ArchiveHeader
	Chunks      []string
	Index       *btree.BTreeG[*common.ClipNode]
	StorageInfo common.ClipStorageInfo
}

func (m *ClipV2Archive) Insert(node *common.ClipNode) {
	m.Index.Set(node)
}

func (m *ClipV2Archive) Get(path string) *common.ClipNode {
	item, ok := m.Index.Get(&common.ClipNode{Path: path})
	if !ok {
		return nil
	}
	return item
}

func (m *ClipV2Archive) ListDirectory(path string) []fuse.DirEntry {
	var entries []fuse.DirEntry

	// Append '/' if not present at the end of the path
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	// Append null character to the path -- if we don't do this we could miss some child nodes.
	// It works because \x00 is lower lexographically than any other character
	pivot := &common.ClipNode{Path: path + "\x00"}
	pathLen := len(path)

	m.Index.Ascend(pivot, func(a *common.ClipNode) bool {
		node := a
		nodePath := node.Path

		// Check if this node path starts with 'path' (meaning it is a child --> continue)
		if len(nodePath) < pathLen || nodePath[:pathLen] != path {
			return true
		}

		// Check if there are any "/" left after removing the prefix
		for i := pathLen; i < len(nodePath); i++ {
			if nodePath[i] == '/' {
				if i == pathLen || nodePath[i-1] != '/' {
					// This node is not an immediate child, continue on
					return true
				}
			}
		}

		// Node is an immediate child, so we append it to entries
		relativePath := nodePath[pathLen:]
		if relativePath != "" {
			entries = append(entries, fuse.DirEntry{
				Mode: node.Attr.Mode,
				Name: relativePath,
			})
		}

		return true
	})

	return entries
}
