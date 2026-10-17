package clip

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

func TestMetadataContractMounted(t *testing.T) {
	if os.Getenv("BEAM_CLIP_METADATA_MOUNTED_TEST") != "1" {
		t.Skip("set BEAM_CLIP_METADATA_MOUNTED_TEST=1 in an isolated Linux FUSE test environment")
	}
	root := metadataContractRoot()
	mountpoint := t.TempDir()
	server, err := fuse.NewServer(fs.NewNodeFS(root, immutableFilesystemOptions()), mountpoint,
		&fuse.MountOptions{Options: []string{"ro"}, DisableXAttrs: true})
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Errorf("unmount fixture: %v", err)
		}
	})
	if err := server.WaitMount(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"closed-dir", "closed-file"} {
		var st syscall.Stat_t
		if err := syscall.Lstat(filepath.Join(mountpoint, name), &st); err != nil {
			t.Fatal(err)
		}
		if st.Mode&07777 != 0 {
			t.Fatalf("mounted %s mode=%#o, want000", name, st.Mode&07777)
		}
	}

	// Read the real getdents inode numbers. os.ReadDir().Info() would stat
	// names and could hide a discrepancy in the directory stream itself.
	dir, err := os.Open(mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	buf := make([]byte, 4096)
	seen := make(map[string]uint64)
	for {
		n, err := unix.ReadDirent(int(dir.Fd()), buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		for pos := 0; pos < n; {
			var layout unix.Dirent
			nameOffset := int(unsafe.Offsetof(layout.Name))
			if n-pos < nameOffset {
				t.Fatal("truncated directory record")
			}
			de := (*unix.Dirent)(unsafe.Pointer(&buf[pos]))
			if int(de.Reclen) <= nameOffset || int(de.Reclen) > n-pos {
				t.Fatal("invalid directory record length")
			}
			nameBytes := buf[pos+nameOffset : pos+int(de.Reclen)]
			end := bytes.IndexByte(nameBytes, 0)
			if end < 0 {
				t.Fatal("unterminated directory name")
			}
			name := string(nameBytes[:end])
			if name != "." && name != ".." {
				var st syscall.Stat_t
				if err := syscall.Lstat(filepath.Join(mountpoint, name), &st); err != nil {
					t.Fatal(err)
				}
				if de.Ino != st.Ino {
					t.Fatalf("getdents %s inode=%d, stat=%d", name, de.Ino, st.Ino)
				}
				seen[name] = de.Ino
			}
			pos += int(de.Reclen)
		}
	}
	if len(seen) != 4 || seen["first"] != 22 || seen["first"] != seen["second"] {
		t.Fatalf("mounted hardlink identity lost: %v", seen)
	}
	for _, name := range []string{"first", "second"} {
		data, err := os.ReadFile(filepath.Join(mountpoint, name))
		if err != nil || string(data) != "payload" {
			t.Fatalf("alias %s data=%q err=%v", name, data, err)
		}
	}
}
