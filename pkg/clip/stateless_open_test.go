package clip

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/btree"
)

type statelessLocalStorage struct {
	*storage.LocalClipStorage
	reads atomic.Uint64
}

type statelessRemoteStorage struct {
	storage.ClipStorageInterface
	data  []byte
	reads int
}

func (s *statelessRemoteStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	s.reads++
	return copy(dest, s.data[off:]), nil
}

func (s *statelessLocalStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	s.reads.Add(1)
	return s.LocalClipStorage.ReadFile(node, dest, off)
}

func newStatelessTestFS(t *testing.T, data []byte) (*ClipFileSystem, *FSNode, *statelessLocalStorage) {
	t.Helper()
	root := &common.ClipNode{Path: "/", NodeType: common.DirNode, Attr: fuse.Attr{Ino: 1, Mode: syscall.S_IFDIR | 0755, Nlink: 2}}
	node := &common.ClipNode{Path: "/file", NodeType: common.FileNode, DataLen: int64(len(data)), Attr: fuse.Attr{Ino: 2, Mode: syscall.S_IFREG | 0644, Size: uint64(len(data)), Nlink: 1}}
	index := btree.New(func(a, b interface{}) bool { return a.(*common.ClipNode).Path < b.(*common.ClipNode).Path })
	index.Set(root)
	index.Set(node)
	archive := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.WriteFile(archive, data, 0600))
	local, err := storage.NewLocalClipStorage(&common.ClipArchiveMetadata{Index: index}, storage.LocalClipStorageOpts{ArchivePath: archive})
	require.NoError(t, err)
	s := &statelessLocalStorage{LocalClipStorage: local}
	cfs, err := NewFileSystem(s, ClipFileSystemOpts{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cfs.closeViewFiles())
		require.NoError(t, s.Cleanup())
	})
	return cfs, &FSNode{filesystem: cfs, clipNode: node, attr: node.Attr}, s
}

func TestStatelessOpenPreservesFDReads(t *testing.T) {
	data := []byte("\x00immutable\xffdata across offsets")
	cfs, node, s := newStatelessTestFS(t, data)
	cfs.root.noOpen = true
	handle, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	require.Nil(t, handle)
	require.Equal(t, syscall.ENOSYS, errno)
	require.Equal(t, syscall.ENOSYS, node.Flush(context.Background(), nil))
	for _, flags := range []int{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_RDONLY | syscall.O_TRUNC, syscall.O_CREAT} {
		_, _, errno := node.Open(context.Background(), uint32(flags))
		require.Equal(t, syscall.EROFS, errno)
	}
	for _, tc := range []struct{ off, size int }{{0, len(data)}, {2, 7}, {len(data) - 3, 10}, {len(data), 8}, {len(data) + 4, 8}, {0, 0}} {
		buf := make([]byte, tc.size)
		result, errno := node.Read(context.Background(), nil, buf, int64(tc.off))
		require.Equal(t, fs.OK, errno)
		got, status := result.Bytes(buf)
		require.Equal(t, fuse.OK, status)
		want := data[min(tc.off, len(data)):min(tc.off+tc.size, len(data))]
		require.True(t, bytes.Equal(want, got), "offset %d, size %d: got %x, want %x", tc.off, tc.size, got, want)
		result.Done()
	}
	require.Zero(t, s.reads.Load(), "nil-handle reads must use the client-local fd path")
	require.Len(t, cfs.viewFiles, 1, "backing file descriptor is shared across reads")
}

func TestStatelessReadFallsBackWithoutFileView(t *testing.T) {
	data := []byte("remote immutable data")
	cfs, node, _ := newStatelessTestFS(t, data)
	node.clipNode.Remote = &common.RemoteRef{ULength: int64(len(data))}
	remote := &statelessRemoteStorage{ClipStorageInterface: cfs.storage, data: data}
	cfs.storage = remote
	buf := make([]byte, 7)
	result, errno := node.Read(context.Background(), nil, buf, 2)
	require.Equal(t, fs.OK, errno)
	got, status := result.Bytes(buf)
	require.Equal(t, fuse.OK, status)
	require.Equal(t, data[2:9], got)
	require.Equal(t, 1, remote.reads)
	result.Done()
}

func TestStatelessReadThroughNodeFSBridge(t *testing.T) {
	data := []byte("bridge without a file handle")
	cfs, _, s := newStatelessTestFS(t, data)
	cfs.root.noOpen = true
	raw := fs.NewNodeFS(cfs.root, immutableFilesystemOptions())
	var entry fuse.EntryOut
	require.Equal(t, fuse.OK, raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, "file", &entry))
	var opened fuse.OpenOut
	require.Equal(t, fuse.ENOSYS, raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: entry.NodeId}}, &opened))
	buf := make([]byte, 7)
	result, status := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: entry.NodeId}, Offset: 2, Size: uint32(len(buf))}, buf)
	require.Equal(t, fuse.OK, status)
	got, status := result.Bytes(buf)
	require.Equal(t, fuse.OK, status)
	require.Equal(t, data[2:9], got)
	result.Done()
	require.Equal(t, fuse.ENOSYS, raw.Flush(nil, &fuse.FlushIn{InHeader: fuse.InHeader{NodeId: entry.NodeId}}))
	require.Zero(t, s.reads.Load())
}

func TestImmutableMountOptionsAreReadOnly(t *testing.T) {
	require.Contains(t, immutableMountOptions(128<<10).Options, "ro")
}
