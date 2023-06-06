package fs

import (
	"context"
	"log"
	"os"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
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
	lock sync.Mutex
}

func (f *FS) Root() (fs.Node, error) {
	return &Dir{fs: f}, nil
}

type Dir struct {
	fs *FS
}

func (d *Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0555
	return nil
}

func (d *Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	d.fs.lock.Lock()
	defer d.fs.lock.Unlock()
	if f, ok := d.fs.files[name]; ok {
		return f, nil
	}
	return nil, fuse.ENOENT
}

func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, res *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	log.Printf("creating a file: %s", req.Name)

	d.fs.lock.Lock()
	defer d.fs.lock.Unlock()
	f := &File{}
	d.fs.files[req.Name] = f
	return f, f, nil
}

func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	a.Inode = 2
	a.Mode = 0666
	a.Size = uint64(len(f.data))
	return nil
}

func (f *File) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	res.Data = f.data[req.Offset : req.Offset+int64(len(f.data))]
	return nil
}

func (f *File) Write(ctx context.Context, req *fuse.WriteRequest, res *fuse.WriteResponse) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.data = append(f.data[0:req.Offset], req.Data...)
	res.Size = len(req.Data)
	return nil
}
