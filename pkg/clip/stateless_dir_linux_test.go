package clip

import (
	"bytes"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

type kernelDirectoryProbe struct {
	*immutableNodeFS
	requests atomic.Int64
}

func (p *kernelDirectoryProbe) OpenDir(c <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	p.requests.Add(1)
	return p.immutableNodeFS.OpenDir(c, in, out)
}
func TestStatelessDirectoryMounted(t *testing.T) {
	if os.Getenv("BEAM_CLIP_STATELESS_DIR_MOUNTED_TEST") != "1" {
		t.Skip("set BEAM_CLIP_STATELESS_DIR_MOUNTED_TEST=1 in an isolated Linux FUSE environment")
	}
	raw, handles := directoryFixture(t, 300)
	raw.noOpenDir = false
	probe := &kernelDirectoryProbe{immutableNodeFS: raw}
	mount := t.TempDir()
	server, err := fuse.NewServer(probe, mount, immutableMountOptions(4096))
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	})
	if err := server.WaitMount(); err != nil {
		t.Fatal(err)
	}
	if !raw.noOpenDir {
		t.Fatal("actual kernel did not negotiate NO_OPENDIR_SUPPORT")
	}
	page := func(fd int) ([]directoryRecord, error) {
		buf := make([]byte, 256)
		n, err := unix.ReadDirent(fd, buf)
		if err != nil {
			return nil, err
		}
		var result []directoryRecord
		for pos := 0; pos < n; {
			de := (*unix.Dirent)(unsafe.Pointer(&buf[pos]))
			base := int(unsafe.Offsetof(de.Name))
			if int(de.Reclen) <= base || pos+int(de.Reclen) > n {
				t.Fatal("invalid mounted dirent")
			}
			name := buf[pos+base : pos+int(de.Reclen)]
			end := bytes.IndexByte(name, 0)
			if end < 0 {
				t.Fatal("unterminated mounted dirent")
			}
			result = append(result, directoryRecord{string(name[:end]), de.Ino, uint64(de.Off), uint32(de.Type) << 12})
			pos += int(de.Reclen)
		}
		return result, nil
	}
	readAll := func() error {
		fd, err := unix.Open(mount, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		seen := map[string]bool{}
		for {
			records, err := page(fd)
			if err != nil {
				return err
			}
			if len(records) == 0 {
				break
			}
			for _, record := range records {
				if record.name != "." && record.name != ".." {
					if seen[record.name] {
						t.Error("duplicate mounted entry")
					}
					seen[record.name] = true
				}
			}
		}
		if len(seen) != len(raw.root.directoryEntries()) {
			t.Errorf("mounted count=%d want=%d", len(seen), len(raw.root.directoryEntries()))
		}
		if _, err = unix.Seek(fd, 0, 0); err != nil {
			return err
		}
		first, err := page(fd)
		if err != nil {
			return err
		}
		if len(first) < 2 {
			t.Fatal("mounted first page too small")
		}
		cookie := first[0].off
		if _, err = unix.Seek(fd, int64(cookie), 0); err != nil {
			return err
		}
		replayed, err := page(fd)
		if err != nil {
			return err
		}
		if len(replayed) == 0 || replayed[0] != first[1] {
			t.Error("mounted seek cookie did not replay next entry")
		}
		return nil
	}
	if err := readAll(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := readAll(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := probe.requests.Load(); got != 1 {
		t.Errorf("kernel OPENDIR calls=%d, want single ENOSYS probe", got)
	}
	handles.mu.Lock()
	defer handles.mu.Unlock()
	if handles.opened == 0 || handles.opened != handles.released || len(handles.active) != 0 {
		t.Errorf("mounted bridge handles opened=%d released=%d active=%d", handles.opened, handles.released, len(handles.active))
	}
}
