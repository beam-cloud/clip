package archive

import "bazil.org/fuse"

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
	Offset   uint64
	Size     uint64
}
