package clip

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type statelessCountFS struct {
	fuse.RawFileSystem
	opens, releases, flushes, reads atomic.Uint64
}

func (f *statelessCountFS) Open(cancel <-chan struct{}, input *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	f.opens.Add(1)
	return f.RawFileSystem.Open(cancel, input, out)
}

func (f *statelessCountFS) Release(cancel <-chan struct{}, input *fuse.ReleaseIn) {
	f.releases.Add(1)
	f.RawFileSystem.Release(cancel, input)
}

func (f *statelessCountFS) Flush(cancel <-chan struct{}, input *fuse.FlushIn) fuse.Status {
	f.flushes.Add(1)
	return f.RawFileSystem.Flush(cancel, input)
}

func (f *statelessCountFS) Read(cancel <-chan struct{}, input *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	f.reads.Add(1)
	return f.RawFileSystem.Read(cancel, input, buf)
}

// Opt in explicitly: missing FUSE or mount privileges then fails instead of
// silently skipping the cache, mmap, and real protocol-message assertions.
func TestStatelessOpenMounted(t *testing.T) {
	if os.Getenv("CLIP_FUSE_TEST") != "1" {
		t.Skip("set CLIP_FUSE_TEST=1 on a Linux host with FUSE and mount privileges")
	}
	for _, stateless := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "stateless"}[stateless], func(t *testing.T) {
			data := bytes.Repeat([]byte("\x00page-cache-and-mmap\xff"), 4096)
			cfs, _, storage := newStatelessTestFS(t, data)
			root, err := cfs.Root()
			require.NoError(t, err)
			var raw fuse.RawFileSystem = fs.NewNodeFS(root, immutableFilesystemOptions())
			if stateless {
				raw = &immutableNodeFS{RawFileSystem: raw, root: cfs.root}
			}
			counts := &statelessCountFS{RawFileSystem: raw}
			mount := t.TempDir()
			server, err := fuse.NewServer(counts, mount, immutableMountOptions(128<<10))
			require.NoError(t, err)
			go server.Serve()
			t.Cleanup(func() { require.NoError(t, server.Unmount()); server.Wait() })
			require.NoError(t, server.WaitMount())
			require.Equal(t, stateless, cfs.root.noOpen)
			file := filepath.Join(mount, "file")
			got, err := os.ReadFile(file)
			require.NoError(t, err)
			require.Equal(t, data, got)
			reads := counts.reads.Load()
			opens := counts.opens.Load()
			const rounds = 32
			for range rounds {
				got, err := os.ReadFile(file)
				require.NoError(t, err)
				require.Equal(t, data, got)
			}
			require.Equal(t, reads, counts.reads.Load(), "reopens must retain kernel-cached data")
			if stateless {
				require.Equal(t, uint64(1), opens, "only the initial OPEN negotiates ENOSYS")
				require.Equal(t, opens, counts.opens.Load())
				require.Zero(t, counts.releases.Load())
				require.LessOrEqual(t, counts.flushes.Load(), uint64(1))
			} else {
				require.Equal(t, uint64(rounds), counts.opens.Load()-opens)
			}
			f, err := os.Open(file)
			require.NoError(t, err)
			defer f.Close()
			buf := make([]byte, 10)
			n, err := f.ReadAt(buf, int64(len(data)-3))
			require.Equal(t, 3, n)
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, data[len(data)-3:], buf[:n])
			n, err = f.ReadAt(buf, int64(len(data)))
			require.Zero(t, n)
			require.ErrorIs(t, err, io.EOF)
			mapped, err := unix.Mmap(int(f.Fd()), 0, len(data), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
			require.NoError(t, err)
			require.Equal(t, data, mapped)
			mapped[0] ^= 0xff
			require.NoError(t, unix.Munmap(mapped))
			got, err = os.ReadFile(file)
			require.NoError(t, err)
			require.Equal(t, data, got, "private writable mmap must not mutate the immutable file")
			for _, flags := range []int{os.O_WRONLY, os.O_RDWR, os.O_RDONLY | os.O_TRUNC, os.O_WRONLY | os.O_CREATE} {
				written, err := os.OpenFile(file, flags, 0600)
				if written != nil {
					written.Close()
				}
				require.True(t, errors.Is(err, syscall.EROFS), "open flags %d: %v", flags, err)
			}
			require.ErrorIs(t, os.WriteFile(filepath.Join(mount, "new"), []byte("x"), 0600), syscall.EROFS)
			require.ErrorIs(t, os.Truncate(file, 0), syscall.EROFS)
			require.Zero(t, storage.reads.Load(), "mounted reads retain the fd-backed fast path")
			stats, err := json.Marshal(map[string]interface{}{"stateless": stateless, "rounds": rounds, "open": counts.opens.Load(), "release": counts.releases.Load(), "flush": counts.flushes.Load(), "read": counts.reads.Load()})
			require.NoError(t, err)
			t.Log(string(stats))
		})
	}
}
