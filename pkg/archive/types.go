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
	DataPos  int64 // Position of the nodes data in the final binary
	DataLen  int64 // Length of the nodes data
}
