package fs

import (
	"context"
	"log"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type Dir struct {
	fs.Inode
	cfs  *ClipFileSystem
	attr fuse.Attr
	// You could add more fields here, such as the directory's name,
	// its metadata, its contents, etc.
}

// func (d *Dir) OnAdd(ctx context.Context) {
// 	log.Println("OnAdd called")

// 	// Set the attributes.
// 	attr := &fuse.AttrOut{}

// 	// Mode is set to a directory plus the permissions 0755.
// 	attr.Attr.Mode = syscall.S_IFDIR | 0755
// 	d.Inode.Mode()
// }

func (d *Dir) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	log.Println("DIR Getattr called")

	log.Println("attr: ", d.attr)
	*out = fuse.AttrOut{
		Attr: d.attr,
	}
	return fs.OK
}

func (d *Dir) OnAdd(ctx context.Context) {
	log.Println("OnAdd called")
}

func (d *Dir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	log.Println("Lookup called")
	return nil, syscall.ENOENT
}

func (d *Dir) Opendir(ctx context.Context) syscall.Errno {
	log.Println("OpenDir called")
	return 0
}

func (d *Dir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	metadata := d.cfs.s.Metadata()
	log.Println("ReadDir called: ", metadata.Get("/log"))
	return nil, syscall.ENOENT
}

func (d *Dir) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	log.Println("Create called")
	return nil, nil, 0, syscall.EROFS
}

func (d *Dir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	log.Println("Mkdir called")
	return nil, syscall.EPERM
}

func (d *Dir) Rmdir(ctx context.Context, name string) syscall.Errno {
	log.Println("Rmdir called")
	return syscall.EPERM
}

func (d *Dir) Unlink(ctx context.Context, name string) syscall.Errno {
	log.Println("Unlink called")
	return fs.OK
}

func (d *Dir) Rename(ctx context.Context, oldName string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	log.Println("Rename called")
	return fs.OK
}
