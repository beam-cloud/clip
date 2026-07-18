package common

import (
	"sort"
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

type StorageMode string

const (
	StorageModeLocal StorageMode = "local"
	StorageModeS3    StorageMode = "s3"
	StorageModeOCI   StorageMode = "oci"
)

// RemoteRef points to a file's data within an OCI layer
type RemoteRef struct {
	LayerDigest string // "sha256:..."
	UOffset     int64  // file payload start in UNCOMPRESSED tar stream
	ULength     int64  // file payload length (uncompressed)
}

type ClipNode struct {
	NodeType    ClipNodeType
	Path        string
	Attr        fuse.Attr
	Target      string
	ContentHash string

	// Legacy fields (keep for back-compat):
	DataPos int64 // Position of the nodes data in the final binary
	DataLen int64 // Length of the nodes data

	// New (v2 read path):
	Remote *RemoteRef
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

func (m *ClipArchiveMetadata) ListDirectory(path string) []fuse.DirEntry {
	var entries []fuse.DirEntry

	// Append '/' if not present at the end of the path
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	// Append null character to the path -- if we don't do this we could miss some child nodes.
	// It works because \x00 is lower lexographically than any other character
	pivot := &ClipNode{Path: path + "\x00"}
	pathLen := len(path)

	m.Index.Ascend(pivot, func(a interface{}) bool {
		node := a.(*ClipNode)
		nodePath := node.Path

		// Matching paths are contiguous in the index. Stop as soon as the
		// traversal leaves this directory instead of scanning the rest of the
		// archive.
		if len(nodePath) < pathLen || nodePath[:pathLen] != path {
			return false
		}

		relativePath := nodePath[pathLen:]
		if relativePath == "" || strings.IndexByte(relativePath, '/') >= 0 {
			return true
		}
		entries = append(entries, fuse.DirEntry{
			Mode: node.Attr.Mode,
			Name: relativePath,
		})

		return true
	})

	return entries
}

// Gzip decompression index.
//
// These checkpoints currently store only compressed and uncompressed offsets.
// They are useful for locating nearby byte positions, but they are not
// independently restartable deflate checkpoints because they do not include the
// inflater bit offset and sliding dictionary needed for true zran-style seek.
type GzipCheckpoint struct {
	COff int64 // Compressed offset
	UOff int64 // Uncompressed offset
}

type GzipIndex struct {
	LayerDigest string
	Checkpoints []GzipCheckpoint // Offset checkpoints every ~2-4 MiB of uncompressed output
}

// Zstd frame index (P1 - future)
type ZstdFrame struct {
	COff int64 // Compressed offset
	CLen int64 // Compressed length
	UOff int64 // Uncompressed offset
	ULen int64 // Uncompressed length
}

type ZstdIndex struct {
	LayerDigest string
	Frames      []ZstdFrame
}

// NearestCheckpoint finds the checkpoint with the largest UOff <= wantU
// This enables efficient seeking by finding the best checkpoint to decompress from
// Uses binary search for O(log n) performance
func NearestCheckpoint(checkpoints []GzipCheckpoint, wantU int64) (cOff, uOff int64) {
	if len(checkpoints) == 0 {
		return 0, 0
	}

	// Binary search: find the first checkpoint with UOff > wantU, then go back one
	i := sort.Search(len(checkpoints), func(i int) bool {
		return checkpoints[i].UOff > wantU
	}) - 1

	if i < 0 {
		return 0, 0
	}

	return checkpoints[i].COff, checkpoints[i].UOff
}
