package clip

import (
	"context"
	"syscall"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/require"
)

func TestStatelessFileOpenKeepsLegacyFallback(t *testing.T) {
	node := &FSNode{filesystem: &ClipFileSystem{}, clipNode: &common.ClipNode{NodeType: common.FileNode}}
	handle, flags, errno := node.Open(context.Background(), syscall.O_RDONLY)
	require.Zero(t, errno)
	require.NotNil(t, handle)
	require.NotZero(t, flags&fuse.FOPEN_KEEP_CACHE)
	node.filesystem.statelessOpens = true
	handle, _, errno = node.Open(context.Background(), syscall.O_RDONLY)
	require.Nil(t, handle)
	require.Equal(t, syscall.ENOSYS, errno)
	require.Equal(t, syscall.ENOSYS, node.Flush(context.Background(), nil))
	require.Equal(t, []string{"ro", "default_permissions"}, immutableImageMountFlags(true))
	require.Nil(t, immutableImageMountFlags(false))
}

func TestStatelessFileReadWithoutHandle(t *testing.T) {
	data := []byte("immutable bytes at an offset")
	_, node, _, _ := newLegacyLocalCacheTestFS(t, data)
	node.filesystem.statelessOpens = true
	result, errno := node.Read(context.Background(), nil, make([]byte, 9), 10)
	require.Zero(t, errno)
	defer result.Done()
	got, status := result.Bytes(make([]byte, 9))
	require.Equal(t, fuse.OK, status)
	require.Equal(t, data[10:19], got)
}

type directoryHandleProbe struct {
	fuse.RawFileSystem
	opened, released int
	got              fuse.ReadIn
	status           fuse.Status
}

func (p *directoryHandleProbe) OpenDir(_ <-chan struct{}, _ *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	p.opened++
	out.Fh = 42
	return fuse.OK
}
func (p *directoryHandleProbe) ReleaseDir(in *fuse.ReleaseIn) {
	if in.Fh != 42 {
		panic("wrong directory handle")
	}
	p.released++
}
func (p *directoryHandleProbe) ReadDir(_ <-chan struct{}, in *fuse.ReadIn, _ *fuse.DirEntryList) fuse.Status {
	p.got = *in
	return p.status
}
func (p *directoryHandleProbe) ReadDirPlus(c <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return p.ReadDir(c, in, out)
}

func TestStatelessDirectoryPreservesCookiesAndReleasesHandles(t *testing.T) {
	for _, plus := range []bool{false, true} {
		for _, status := range []fuse.Status{fuse.OK, fuse.EIO} {
			probe := &directoryHandleProbe{status: status}
			fs := &immutableDirectoryFS{RawFileSystem: probe, stateless: true}
			input := &fuse.ReadIn{Fh: 0, Offset: 73}
			var got fuse.Status
			if plus {
				got = fs.ReadDirPlus(nil, input, nil)
			} else {
				got = fs.ReadDir(nil, input, nil)
			}
			require.Equal(t, status, got)
			require.EqualValues(t, 42, probe.got.Fh)
			require.EqualValues(t, 73, probe.got.Offset)
			require.EqualValues(t, 0, input.Fh)
			require.Equal(t, 1, probe.opened)
			require.Equal(t, 1, probe.released)
		}
	}
	probe := &directoryHandleProbe{}
	fs := &immutableDirectoryFS{RawFileSystem: probe}
	require.Equal(t, fuse.OK, fs.ReadDir(nil, &fuse.ReadIn{Fh: 8, Offset: 9}, nil))
	require.Zero(t, probe.opened)
	require.Zero(t, probe.released)
	require.EqualValues(t, 8, probe.got.Fh)
	require.Equal(t, fuse.OK, fs.OpenDir(nil, &fuse.OpenIn{}, &fuse.OpenOut{}))
	require.Equal(t, 1, probe.opened)
	fs.stateless = true
	require.Equal(t, fuse.ENOSYS, fs.OpenDir(nil, &fuse.OpenIn{}, &fuse.OpenOut{}))
	require.Equal(t, 1, probe.opened)
}
