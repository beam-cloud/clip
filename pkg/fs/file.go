package fs

import (
	"context"
	"log"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type File struct {
	fs.Inode
	fsys *FileSystem
	// You could add more fields here, such as the file's name,
	// its metadata, its contents, etc.
}

func (f *File) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	log.Println("Open called")
	return f, 0, fs.OK
}

func (f *File) Release(ctx context.Context, flags uint32) syscall.Errno {
	log.Println("Release called")
	return fs.OK
}

func (f *File) Getattr(ctx context.Context, out *fuse.AttrOut, file fs.FileHandle, h *fuse.InHeader) syscall.Errno {
	log.Println("Getattr called")
	return fs.OK
}

func (f *File) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	log.Println("Setattr called")
	return fs.OK
}

func (f *File) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	log.Println("Read called")
	return nil, fs.OK
}

func (f *File) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	log.Println("Write called")
	return 0, fs.OK
}

func (f *File) Truncate(ctx context.Context, size int64) syscall.Errno {
	log.Println("Truncate called")
	return fs.OK
}

func (f *File) Unlink(ctx context.Context, name string) syscall.Errno {
	log.Println("Unlink called")
	return fs.OK
}

func (f *File) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	log.Println("Rename called")
	return fs.OK
}

func (f *File) Flush(ctx context.Context, file fs.FileHandle) syscall.Errno {
	log.Println("Flush called")
	return fs.OK
}

func (f *File) Fsync(ctx context.Context, file fs.FileHandle, flags uint32) syscall.Errno {
	log.Println("Fsync called")
	return fs.OK
}

func (f *File) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	log.Println("Readlink called")
	return nil, fs.OK
}

func (f *File) Symlink(ctx context.Context, target string, name string) syscall.Errno {
	log.Println("Symlink called")
	return fs.OK
}

func (f *File) Link(ctx context.Context, target fs.InodeEmbedder, name string) syscall.Errno {
	log.Println("Link called")
	return fs.OK
}

func (f *File) Rmdir(ctx context.Context, name string) syscall.Errno {
	log.Println("Rmdir called")
	return fs.OK
}

func (f *File) Opendir(ctx context.Context) syscall.Errno {
	log.Println("Opendir called")
	return fs.OK
}

func (f *File) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	log.Println("Readdir called")
	return nil, fs.OK
}

func (f *File) Releasedir(ctx context.Context) syscall.Errno {
	log.Println("Releasedir called")
	return fs.OK
}
