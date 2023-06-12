package clipfs

import (
	"context"
	"log"
	"path"
	"syscall"

	"github.com/beam-cloud/clip/pkg/archive"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type FSNode struct {
	fs.Inode
	cfs      *ClipFileSystem
	clipNode *archive.ClipNode
	attr     fuse.Attr
}

func (n *FSNode) OnAdd(ctx context.Context) {
	log.Println("OnAdd called for path: ", n.clipNode.Path)

	// Fetch the node attributes from your storage
	node := n.cfs.s.Metadata().Get(n.clipNode.Path)
	if node != nil {
		n.attr.Ino = node.Attr.Ino
		n.attr.Size = node.Attr.Size
		n.attr.Blocks = node.Attr.Blocks
		n.attr.Atime = node.Attr.Atime
		n.attr.Mtime = node.Attr.Mtime
		n.attr.Ctime = node.Attr.Ctime
		n.attr.Mode = node.Attr.Mode
		n.attr.Nlink = node.Attr.Nlink
		n.attr.Owner = node.Attr.Owner
	}
}

func (n *FSNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	log.Println("Getattr called for path: ", n.clipNode.Path)

	// Fetch the node attributes
	node := n.cfs.s.Metadata().Get(n.clipNode.Path)
	if node == nil {
		return syscall.ENOENT
	}

	// Fill in the AttrOut struct
	out.Ino = node.Attr.Ino
	out.Size = node.Attr.Size
	out.Blocks = node.Attr.Blocks
	out.Atime = node.Attr.Atime
	out.Mtime = node.Attr.Mtime
	out.Ctime = node.Attr.Ctime
	out.Mode = node.Attr.Mode
	out.Nlink = node.Attr.Nlink
	out.Owner = node.Attr.Owner

	return fs.OK
}

func (n *FSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	// Create the full path of the child node
	childPath := path.Join(n.clipNode.Path, name)

	// Lookup the child node
	child := n.cfs.s.Metadata().Get(childPath)
	if child == nil {
		// No child with the requested name exists
		return nil, syscall.ENOENT
	}

	// Create a new Inode for the child
	childInode := n.NewInode(ctx, &FSNode{cfs: n.cfs, clipNode: child, attr: child.Attr}, fs.StableAttr{Mode: child.Attr.Mode, Ino: child.Attr.Ino})
	return childInode, fs.OK
}

func (n *FSNode) Opendir(ctx context.Context) syscall.Errno {
	log.Println("OpenDir called")
	return 0
}

func (n *FSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	log.Println("ReadDir called")
	dirEntries := n.cfs.s.Metadata().ListDirectory(n.clipNode.Path)
	return fs.NewListDirStream(dirEntries), fs.OK
}

func (n *FSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	log.Println("Create called")
	return nil, nil, 0, syscall.EROFS
}

func (n *FSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	log.Println("Mkdir called")
	return nil, syscall.EPERM
}

func (n *FSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	log.Println("Rmdir called")
	return syscall.EPERM
}

func (n *FSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	log.Println("Unlink called")
	return fs.OK
}

func (n *FSNode) Rename(ctx context.Context, oldName string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	log.Println("Rename called")
	return fs.OK
}
