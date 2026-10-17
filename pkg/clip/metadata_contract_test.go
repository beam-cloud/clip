package clip

import (
	"context"
	"syscall"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type metadataContractStorage struct{ metadata *common.ClipArchiveMetadata }

func (s metadataContractStorage) Metadata() *common.ClipArchiveMetadata { return s.metadata }
func (s metadataContractStorage) CachedLocally() bool                   { return true }
func (s metadataContractStorage) Cleanup() error                        { return nil }
func (s metadataContractStorage) ReadFile(n *common.ClipNode, dest []byte, off int64) (int, error) {
	return copy(dest, []byte("payload")[off:]), nil
}

func metadataContractRoot() *FSNode {
	index := NewClipArchiver().newIndex()
	nodes := []*common.ClipNode{
		{Path: "/", NodeType: common.DirNode, Attr: fuse.Attr{Ino: 1, Mode: syscall.S_IFDIR | 0755, Nlink: 2}},
		{Path: "/closed-dir", NodeType: common.DirNode, Attr: fuse.Attr{Ino: 10, Mode: syscall.S_IFDIR, Nlink: 2}},
		{Path: "/closed-file", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 11, Mode: syscall.S_IFREG, Nlink: 1}},
		{Path: "/first", NodeType: common.FileNode, DataLen: 7, Attr: fuse.Attr{Ino: 22, Mode: syscall.S_IFREG | 0640, Nlink: 2, Size: 7}},
		{Path: "/second", NodeType: common.FileNode, DataLen: 7, Attr: fuse.Attr{Ino: 22, Mode: syscall.S_IFREG | 0640, Nlink: 2, Size: 7}},
	}
	for _, n := range nodes {
		index.Set(n)
	}
	metadata := &common.ClipArchiveMetadata{Index: index}
	filesystem := &ClipFileSystem{storage: metadataContractStorage{metadata}, lookupCache: make(map[string]*lookupCacheEntry)}
	root := &FSNode{filesystem: filesystem, clipNode: nodes[0], attr: nodes[0].Attr}
	filesystem.root = root
	return root
}

func TestMetadataContractNullPermissions(t *testing.T) {
	for _, name := range []string{"closed-dir", "closed-file"} {
		t.Run(name, func(t *testing.T) {
			root := metadataContractRoot()
			raw := fs.NewNodeFS(root, immutableFilesystemOptions())
			var entry fuse.EntryOut
			if status := raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, name, &entry); status != fuse.OK {
				t.Fatal(status)
			}
			if entry.Mode&07777 != 0 {
				t.Fatalf("lookup elevated mode000 to %#o", entry.Mode&07777)
			}
			var attr fuse.AttrOut
			if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: entry.NodeId}}, &attr); status != fuse.OK {
				t.Fatal(status)
			}
			if attr.Mode != entry.Mode {
				t.Fatalf("getattr mode %#o != lookup %#o", attr.Mode, entry.Mode)
			}
			for _, mask := range []uint32{fuse.R_OK, fuse.W_OK, fuse.X_OK} {
				in := fuse.AccessIn{InHeader: fuse.InHeader{NodeId: entry.NodeId, Caller: fuse.Caller{Owner: fuse.Owner{Uid: 2345, Gid: 2345}}}, Mask: mask}
				if status := raw.Access(nil, &in); status != fuse.EACCES {
					t.Fatalf("access(%d)=%v, want EACCES", mask, status)
				}
			}
		})
	}
}

func TestMetadataContractReaddirInodesMatchLookup(t *testing.T) {
	root := metadataContractRoot()
	raw := fs.NewNodeFS(root, immutableFilesystemOptions())
	entries := root.filesystem.storage.Metadata().ListDirectory("/")
	seen := make(map[string]uint64)
	for _, de := range entries {
		var out fuse.EntryOut
		if status := raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, de.Name, &out); status != fuse.OK {
			t.Fatal(status)
		}
		if de.Ino == 0 || de.Ino != out.Ino {
			t.Fatalf("readdir %s inode %d != lookup %d", de.Name, de.Ino, out.Ino)
		}
		var attr fuse.AttrOut
		if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: out.NodeId}}, &attr); status != fuse.OK {
			t.Fatal(status)
		}
		if attr.Ino != de.Ino {
			t.Fatalf("getattr %s inode %d != readdir %d", de.Name, attr.Ino, de.Ino)
		}
		seen[de.Name] = de.Ino
	}
	if len(seen) != 4 || seen["first"] != 22 || seen["first"] != seen["second"] {
		t.Fatalf("hardlink identity lost: %v", seen)
	}
	// A second lookup exercises CLIP's own inode cache as well as go-fuse's
	// stable-inode deduplication; neither may change a listed inode number.
	for name, ino := range seen {
		var out fuse.EntryOut
		if _, errno := root.Lookup(context.Background(), name, &out); errno != 0 || out.Ino != ino {
			t.Fatalf("cached lookup %s: inode=%d errno=%v", name, out.Ino, errno)
		}
	}
}
