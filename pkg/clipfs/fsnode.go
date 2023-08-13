package clipfs

import (
	"context"
	"fmt"
	"log"
	"path"
	"syscall"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type FSNode struct {
	fs.Inode
	filesystem *ClipFileSystem
	clipNode   *common.ClipNode
	attr       fuse.Attr
}

func (n *FSNode) log(format string, v ...interface{}) {
	if n.filesystem.verbose {
		log.Printf(fmt.Sprintf("[CLIPFS] (%s) %s", n.clipNode.Path, format), v...)
	}
}

func (n *FSNode) OnAdd(ctx context.Context) {
	n.log("OnAdd called")
}

func (n *FSNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.log("Getattr called")

	// Fetch the node attributes
	node := n.filesystem.s.Metadata().Get(n.clipNode.Path)
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
	n.log("Lookup called with name: %s", name)

	// Create the full path of the child node
	childPath := path.Join(n.clipNode.Path, name)

	// Check the cache
	n.filesystem.cacheMutex.RLock()
	entry, found := n.filesystem.lookupCache[childPath]
	n.filesystem.cacheMutex.RUnlock()
	if found {
		n.log("Lookup cache hit for name: %s", childPath)
		out.Attr = entry.attr
		return entry.inode, fs.OK
	}

	// Lookup the child node
	child := n.filesystem.s.Metadata().Get(childPath)
	if child == nil {
		// No child with the requested name exists
		return nil, syscall.ENOENT
	}

	// Fill out the child node's attributes
	out.Attr = child.Attr

	// Create a new Inode for the child
	childInode := n.NewInode(ctx, &FSNode{filesystem: n.filesystem, clipNode: child, attr: child.Attr}, fs.StableAttr{Mode: child.Attr.Mode, Ino: child.Attr.Ino})

	// Cache the result
	n.filesystem.cacheMutex.Lock()
	n.filesystem.lookupCache[childPath] = &cacheEntry{inode: childInode, attr: child.Attr}
	n.filesystem.cacheMutex.Unlock()

	return childInode, fs.OK
}

func (n *FSNode) Opendir(ctx context.Context) syscall.Errno {
	n.log("Opendir called")
	return 0
}

func (n *FSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	n.log("Open called with flags: %v", flags)
	return nil, 0, fs.OK
}

func (n *FSNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n.log("Read called with offset: %v", off)

	nRead, err := n.filesystem.s.ReadFile(n.clipNode, dest, off)
	if err != nil {
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[:nRead]), fs.OK
}

func (n *FSNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	n.log("Getxattr called with attr: %s", attr)

	// TODO: Implement xattr logic
	return 0, syscall.ENODATA
}

func (n *FSNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	n.log("Readlink called")

	if n.clipNode.NodeType != common.SymLinkNode {
		// This node is not a symlink
		return nil, syscall.EINVAL
	}

	// Use the symlink target path directly
	symlinkTarget := n.clipNode.Target

	// In this case, we don't need to read the file
	return []byte(symlinkTarget), fs.OK
}

func (n *FSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	n.log("Readdir called")

	dirEntries := n.filesystem.s.Metadata().ListDirectory(n.clipNode.Path)
	return fs.NewListDirStream(dirEntries), fs.OK
}

func (n *FSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	n.log("Create called with name: %s, flags: %v, mode: %v", name, flags, mode)
	return nil, nil, 0, syscall.EROFS
}

func (n *FSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.log("Mkdir called with name: %s, mode: %v", name, mode)
	return nil, syscall.EROFS
}

func (n *FSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.log("Rmdir called with name: %s", name)
	return syscall.EROFS
}

func (n *FSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.log("Unlink called with name: %s", name)
	return syscall.EROFS
}

func (n *FSNode) Rename(ctx context.Context, oldName string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.log("Rename called with oldName: %s, newName: %s, flags: %v", oldName, newName, flags)
	return syscall.EROFS
}
