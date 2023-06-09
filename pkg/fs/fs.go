package fs

import (
	"context"
	"sync"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type FS struct {
	files map[string]*File
	lock  sync.Mutex
}

func NewFS() *FS {
	return &FS{
		files: make(map[string]*File),
	}
}

type File struct {
	data []byte
	link string
	lock sync.Mutex
	fs.Inode
}

type Root struct {
	fs *FS
	fs.Inode
}

func (f *FS) Root() (fs.InodeEmbedder, fuse.Status) {
	root := &Root{fs: f}
	return root, fuse.OK
}

func (d *Root) Lookup(name string, out *fuse.EntryOut) (*fs.Inode, fuse.Status) {
	d.fs.lock.Lock()
	defer d.fs.lock.Unlock()
	if f, ok := d.fs.files[name]; ok {
		return &f.Inode, fuse.OK
	}
	return nil, fuse.ENOENT
}

func (d *Root) Symlink(name string, target string, out *fuse.EntryOut) (*fs.Inode, fuse.Status) {
	d.fs.lock.Lock()
	defer d.fs.lock.Unlock()
	f := &File{link: target}
	d.fs.files[name] = f
	return &f.Inode, fuse.OK
}

func (d *Root) Create(name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fs.Inode, fh fs.FileHandle, fuseFlags uint32, status fuse.Status) {
	d.fs.lock.Lock()
	defer d.fs.lock.Unlock()
	f := &File{}
	d.fs.files[name] = f
	return &f.Inode, f, 0, fuse.OK
}

func (f *File) Readlink(ctx context.Context) ([]byte, fuse.Status) {
	f.lock.Lock()
	defer f.lock.Unlock()
	return []byte(f.link), fuse.OK
}

func (f *File) GetAttr(out *fuse.AttrOut, file fs.FileHandle, context *fuse.Context) fuse.Status {
	f.lock.Lock()
	defer f.lock.Unlock()
	out.Mode = fuse.S_IFREG | 0666
	out.Size = uint64(len(f.data))
	return fuse.OK
}

func (f *File) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, fuse.Status) {
	f.lock.Lock()
	defer f.lock.Unlock()
	end := off + int64(len(dest))
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}

	return fuse.ReadResultData(f.data[off:end]), fuse.OK
}

func (f *File) Write(ctx context.Context, data []byte, off int64) (uint32, fuse.Status) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.data = append(f.data[0:off], data...)
	return uint32(len(data)), fuse.OK
}
