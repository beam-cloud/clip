package common

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
	NodeType    ClipNodeType
	Path        string
	Attr        fuse.Attr
	Target      string
	ContentHash string
	DataPos     int64 // Position of the nodes data in the final binary
	DataLen     int64 // Length of the nodes data
}

// IsDir returns true if the ClipNode represents a directory.
func (n *ClipNode) IsDir() bool {
	return n.NodeType == DirNode
}

// IsSymlink returns true if the ClipNode represents a symlink.
func (n *ClipNode) IsSymlink() bool {
	return n.NodeType == SymLinkNode
}

type ClipArchiveMetadata struct {
	Header      ClipArchiveHeader
	Index       *btree.BTree
	StorageInfo ClipStorageInfo
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

type CacheEntry struct {
	Path    string
	Entries []fuse.DirEntry
}

var directoryCache = map[string]CacheEntry{}

func (m *ClipArchiveMetadata) ListDirectory(path string) []fuse.DirEntry {
	// Append '/' if not present at the end of the pat
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	// Check dir cache first
	if entry, found := directoryCache[path]; found {
		return entry.Entries
	}

	// Append null character to the path -- if we don't do this we could miss some child nodes.
	// It works because \x00 is lower lexographically than any other character
	pivot := &ClipNode{Path: path + "\x00"}
	pathLen := len(path)
	var entries []fuse.DirEntry

	m.Index.Ascend(pivot, func(a interface{}) bool {
		node := a.(*ClipNode)

		// Check if this node path starts with 'path' (meaning it is a child --> continue
		if len(node.Path) < pathLen || node.Path[:pathLen] != path {
			return true
		}

		// Check if there are any "/" left after removing the prefix
		for i := pathLen; i < len(node.Path); i++ {
			if node.Path[i] == '/' {
				if i == pathLen || node.Path[i-1] != '/' {
					// This node is not an immediate child, continue on
					return true
				}
			}
		}

		relativePath := node.Path[pathLen:]

		// Only add if there is a non-empty relative path without any further slashes
		if relativePath != "" {
			entries = append(entries, fuse.DirEntry{
				Mode: node.Attr.Mode,
				Name: relativePath,
			})
		}
		return true
	})

	// Update cache with the new list of entries
	directoryCache[path] = CacheEntry{Path: path, Entries: entries}

	return entries
}
