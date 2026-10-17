package clip

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type directoryProbe struct {
	fuse.RawFileSystem
	mu                      sync.Mutex
	opened, released, reads int
	active                  map[uint64]bool
	openErr, readErr        fuse.Status
	header                  fuse.InHeader
	readInput               fuse.ReadIn
	cancel                  <-chan struct{}
}

func (p *directoryProbe) OpenDir(c <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.openErr != 0 {
		return p.openErr
	}
	status := p.RawFileSystem.OpenDir(c, in, out)
	if status == fuse.OK {
		p.opened++
		p.active[out.Fh] = true
		p.header = in.InHeader
	}
	return status
}
func (p *directoryProbe) read(c <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList, plus bool) fuse.Status {
	p.mu.Lock()
	p.reads++
	p.readInput = *in
	p.cancel = c
	status := p.readErr
	p.mu.Unlock()
	if status != 0 {
		return status
	}
	if plus {
		return p.RawFileSystem.ReadDirPlus(c, in, out)
	}
	return p.RawFileSystem.ReadDir(c, in, out)
}
func (p *directoryProbe) ReadDir(c <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return p.read(c, in, out, false)
}
func (p *directoryProbe) ReadDirPlus(c <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return p.read(c, in, out, true)
}
func (p *directoryProbe) ReleaseDir(in *fuse.ReleaseIn) {
	p.RawFileSystem.ReleaseDir(in)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released++
	delete(p.active, in.Fh)
}
func directoryFixture(t *testing.T, count int) (*immutableNodeFS, *directoryProbe) {
	t.Helper()
	root := metadataContractRoot()
	for i := 0; i < count; i++ {
		n := &common.ClipNode{Path: fmt.Sprintf("/file-%04d", i), NodeType: common.FileNode, Attr: fuse.Attr{Ino: uint64(100 + i), Mode: syscall.S_IFREG | 0640, Nlink: 1, Size: 7}, DataLen: 7}
		root.filesystem.storage.Metadata().Index.Set(n)
	}
	probe := &directoryProbe{RawFileSystem: fs.NewNodeFS(root, immutableFilesystemOptions()), active: make(map[uint64]bool)}
	return &immutableNodeFS{RawFileSystem: probe, root: root, noOpenDir: true}, probe
}

type directoryRecord struct {
	name     string
	ino, off uint64
	mode     uint32
}

func readDirectoryPage(t *testing.T, raw *immutableNodeFS, off uint64, plus bool) ([]directoryRecord, uint64) {
	t.Helper()
	data := make([]byte, 512)
	out := fuse.NewDirEntryList(data, off)
	in := fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 1}, Offset: off, Size: uint32(len(data))}
	var status fuse.Status
	if plus {
		status = raw.ReadDirPlus(nil, &in, out)
	} else {
		status = raw.ReadDir(nil, &in, out)
	}
	if status != fuse.OK {
		t.Fatalf("read offset=%d plus=%t: %v", off, plus, status)
	}
	prefix := 0
	if plus {
		prefix = int(unsafe.Sizeof(fuse.EntryOut{}))
	}
	var records []directoryRecord
	for pos := 0; pos+prefix+24 <= len(data); {
		entry := data[pos+prefix:]
		n := int(binary.LittleEndian.Uint32(entry[16:20]))
		if n == 0 {
			break
		}
		length := (24 + n + 7) &^ 7
		if n > 255 || pos+prefix+length > len(data) {
			t.Fatal("invalid directory response")
		}
		record := directoryRecord{string(entry[24 : 24+n]), binary.LittleEndian.Uint64(entry[:8]), binary.LittleEndian.Uint64(entry[8:16]), binary.LittleEndian.Uint32(entry[20:24]) << 12}
		if plus {
			attr := (*fuse.EntryOut)(unsafe.Pointer(&data[pos]))
			if attr.Ino != record.ino || attr.NodeId == 0 || attr.Mode&syscall.S_IFMT != record.mode {
				t.Fatal("PLUS inode/attribute mismatch")
			}
		}
		records = append(records, record)
		pos += prefix + length
	}
	return records, out.Offset
}
func TestStatelessDirectoryBridgePagination(t *testing.T) {
	for _, plus := range []bool{false, true} {
		t.Run(fmt.Sprintf("plus=%t", plus), func(t *testing.T) {
			raw, p := directoryFixture(t, 137)
			if s := raw.OpenDir(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: 1}}, &fuse.OpenOut{}); s != fuse.ENOSYS {
				t.Fatal(s)
			}
			var all []directoryRecord
			var off uint64
			for page := 0; page < 150; page++ {
				records, next := readDirectoryPage(t, raw, off, plus)
				if len(records) == 0 {
					break
				}
				replay, _ := readDirectoryPage(t, raw, off, plus)
				if !reflect.DeepEqual(records, replay) {
					t.Fatal("replayed cookie changed response")
				}
				if next <= off {
					t.Fatal("cookie did not advance")
				}
				all = append(all, records...)
				off = next
			}
			expected := raw.root.directoryEntries()
			if len(all) != len(expected) {
				t.Fatalf("entries=%d want=%d", len(all), len(expected))
			}
			for i, e := range expected {
				if all[i].name != e.Name || all[i].ino != e.Ino || all[i].off != uint64(i+1) {
					t.Fatalf("entry %d changed", i)
				}
			}
			rewind, _ := readDirectoryPage(t, raw, 0, plus)
			if len(rewind) == 0 || rewind[0] != all[0] {
				t.Fatal("rewind failed")
			}
			in := fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 1}, Offset: uint64(len(expected) + 1)}
			if s := raw.ReadDir(nil, &in, fuse.NewDirEntryList(make([]byte, 512), in.Offset)); s != fuse.EINVAL {
				t.Fatalf("invalid cookie=%v", s)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.opened == 0 || p.opened != p.released || len(p.active) != 0 {
				t.Fatalf("handles opened=%d released=%d active=%d", p.opened, p.released, len(p.active))
			}
		})
	}
}
func TestStatelessDirectoryConcurrentReaders(t *testing.T) {
	raw, p := directoryFixture(t, 100)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				records, _ := readDirectoryPage(t, raw, uint64(j), j%2 == 0)
				if len(records) == 0 {
					t.Error("empty concurrent page")
				}
			}
		}()
	}
	wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opened != p.released || len(p.active) != 0 {
		t.Fatal("concurrent handle leak")
	}
}
func TestStatelessDirectoryCallerAndFailures(t *testing.T) {
	raw, p := directoryFixture(t, 3)
	header := fuse.InHeader{NodeId: 1, Unique: 34, Caller: fuse.Caller{Owner: fuse.Owner{Uid: 2345, Gid: 3456}, Pid: 4567}}
	in := fuse.ReadIn{InHeader: header, Offset: 2, Size: 512, ReadFlags: 3, LockOwner: 88, Flags: syscall.O_RDONLY | syscall.O_NONBLOCK}
	original := in
	if s := raw.ReadDir(nil, &in, fuse.NewDirEntryList(make([]byte, 512), in.Offset)); s != fuse.OK {
		t.Fatal(s)
	}
	if in != original || p.header != header || p.readInput.Fh == 0 {
		t.Fatal("input/caller changed")
	}
	copied := p.readInput
	copied.Fh = 0
	if copied != in {
		t.Fatal("read fields changed")
	}
	cancel := make(chan struct{})
	close(cancel)
	for _, status := range []fuse.Status{fuse.EINTR, fuse.EIO} {
		p.readErr = status
		if s := raw.ReadDirPlus(cancel, &in, fuse.NewDirEntryList(make([]byte, 512), in.Offset)); s != status {
			t.Fatal(s)
		}
		if p.cancel != cancel {
			t.Fatal("cancellation channel replaced")
		}
	}
	p.readErr = 0
	p.openErr = fuse.EACCES
	opened := p.opened
	if s := raw.ReadDir(nil, &in, fuse.NewDirEntryList(make([]byte, 512), in.Offset)); s != fuse.EACCES {
		t.Fatal(s)
	}
	if p.opened != opened || p.opened != p.released || len(p.active) != 0 {
		t.Fatal("error handle leak")
	}
}
func TestStatelessDirectoryFallbackAndOtherOperations(t *testing.T) {
	raw, p := directoryFixture(t, 3)
	if immutableMountOptions(4096).ExtraCapabilities != immutableOpenDirCapability {
		t.Fatal("capability not advertised")
	}
	raw.noOpenDir = false
	var opened fuse.OpenOut
	header := fuse.InHeader{NodeId: 1}
	if s := raw.OpenDir(nil, &fuse.OpenIn{InHeader: header}, &opened); s != fuse.OK || opened.Fh == 0 {
		t.Fatal("fallback open", s)
	}
	in := fuse.ReadIn{InHeader: header, Fh: opened.Fh, Size: 512}
	// A handle created before capability activation keeps the normal path.
	raw.noOpenDir = true
	if s := raw.ReadDir(nil, &in, fuse.NewDirEntryList(make([]byte, 512), 0)); s != fuse.OK || p.opened != 1 {
		t.Fatal("nonzero handle not delegated", s)
	}
	raw.ReleaseDir(&fuse.ReleaseIn{InHeader: header, Fh: opened.Fh})
	if s := raw.FsyncDir(nil, &fuse.FsyncIn{InHeader: header}); s != fuse.ENOTSUP {
		t.Fatal("fsync changed", s)
	}
	if s := raw.Ioctl(nil, &fuse.IoctlIn{InHeader: header, Flags: fuse.IOCTL_DIR}, nil, &fuse.IoctlOut{}, nil); s != fuse.Status(syscall.ENOTTY) {
		t.Fatal("ioctl changed", s)
	}
	// Access remains bridge-controlled and restrictive metadata cannot be bypassed.
	var entry fuse.EntryOut
	if s := raw.Lookup(nil, &header, "closed-dir", &entry); s != fuse.OK {
		t.Fatal(s)
	}
	if s := raw.Access(nil, &fuse.AccessIn{InHeader: fuse.InHeader{NodeId: entry.NodeId, Caller: fuse.Caller{Owner: fuse.Owner{Uid: 2345, Gid: 2345}}}, Mask: fuse.R_OK}); s != fuse.EACCES {
		t.Fatal("permission changed", s)
	}
	if p.opened != p.released || len(p.active) != 0 {
		t.Fatal("fallback handle leak")
	}
}
