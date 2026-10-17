package clip

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/hanwen/go-fuse/v2/fuse"
	"io"
	"reflect"
	"syscall"
	"testing"
)

func hardlinkFixture(t *testing.T) ([]byte, *LayerArtifact) {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, h := range []*tar.Header{{Name: "public", Typeflag: tar.TypeReg, Mode: 0640, Uid: 1001, Gid: 1002, Size: 7}, {Name: "alias", Typeflag: tar.TypeLink, Linkname: "public"}} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	ca := NewClipArchiver()
	a, err := ca.indexLayerToArtifact(context.Background(), io.NopCloser(bytes.NewReader(raw.Bytes())), "sha256:synthetic-contract", IndexOCIImageOptions{CheckpointMiB: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return raw.Bytes(), a
}
func assertLinkGroup(t *testing.T, ca *ClipArchiver, idx interface{ Get(interface{}) interface{} }, paths []string, want uint32) {
	t.Helper()
	var inode uint64
	for _, p := range paths {
		n := idx.Get(&common.ClipNode{Path: p}).(*common.ClipNode)
		if inode == 0 {
			inode = n.Attr.Ino
		}
		if n.Attr.Ino != inode || n.Attr.Nlink != want {
			t.Fatalf("group %v: path %s ino_same=%t nlink=%d want=%d", paths, p, n.Attr.Ino == inode, n.Attr.Nlink, want)
		}
	}
}
func TestOCIImageVisibleHardLinkCounts(t *testing.T) {
	raw, _ := hardlinkFixture(t)
	layer, err := tarball.LayerFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	lp, err := layout.Write(root, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.AppendImage(image); err != nil {
		t.Fatal(err)
	}
	ca := NewClipArchiver()
	idx, _, _, _, _, _, _, _, err := ca.IndexOCIImage(context.Background(), IndexOCIImageOptions{ImageRef: "example.invalid/synthetic:tag", LocalLayoutPath: root, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	assertLinkGroup(t, ca, idx, []string{"/public", "/alias"}, 2)
	for _, p := range []string{"/public", "/alias"} {
		n := idx.Get(&common.ClipNode{Path: p}).(*common.ClipNode)
		if n.Attr.Uid != 1001 || n.Attr.Gid != 1002 || n.Attr.Mode != syscall.S_IFREG|0640 || n.Remote == nil || n.Remote.ULength != 7 {
			t.Fatalf("other metadata changed: %s", p)
		}
	}
}
func TestVisibleHardLinkCountRecount(t *testing.T) {
	cases := []struct {
		name  string
		next  []LayerEntry
		paths []string
		want  uint32
	}{
		{"three aliases", []LayerEntry{{Kind: LayerEntryHardLink, Path: "/third", Target: "/public"}}, []string{"/public", "/alias", "/third"}, 3},
		{"overwrite alias same target", []LayerEntry{{Kind: LayerEntryHardLink, Path: "/alias", Target: "/public"}}, []string{"/public", "/alias"}, 2},
		{"whiteout original", []LayerEntry{{Kind: LayerEntryWhiteout, Path: "/public"}}, []string{"/alias"}, 1},
		{"whiteout alias", []LayerEntry{{Kind: LayerEntryWhiteout, Path: "/alias"}}, []string{"/public"}, 1},
		{"whiteout and regain alias", []LayerEntry{{Kind: LayerEntryWhiteout, Path: "/alias"}, {Kind: LayerEntryHardLink, Path: "/alias", Target: "/public"}}, []string{"/public", "/alias"}, 2},
		{"opaque subtree aliases", []LayerEntry{{Kind: LayerEntryHardLink, Path: "/dir/third", Target: "/public"}, {Kind: LayerEntryOpaqueWhiteout, Path: "/dir"}}, []string{"/public", "/alias"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, a := hardlinkFixture(t)
			ca := NewClipArchiver()
			idx := ca.newIndex()
			ca.applyLayerArtifact(idx, a)
			ca.applyLayerArtifact(idx, &LayerArtifact{Entries: c.next})
			normalizeFileLinkCounts(idx, nil)
			assertLinkGroup(t, ca, idx, c.paths, c.want)
			first := idx.Get(&common.ClipNode{Path: c.paths[0]}).(*common.ClipNode)
			before := *first
			normalizeFileLinkCounts(idx, nil)
			if !reflect.DeepEqual(before, *first) {
				t.Fatal("recount is not idempotent")
			}
		})
	}
}
func TestVisibleHardLinkReplacementAndNonFiles(t *testing.T) {
	_, a := hardlinkFixture(t)
	ca := NewClipArchiver()
	idx := ca.newIndex()
	ca.applyLayerArtifact(idx, a)
	ca.applyLayerArtifact(idx, &LayerArtifact{Entries: []LayerEntry{{Kind: LayerEntryHardLink, Path: "/third", Target: "/public"}, {Kind: LayerEntryNode, Node: &common.ClipNode{Path: "/public", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 999, Mode: syscall.S_IFREG | 0600, Nlink: 99}, Remote: &common.RemoteRef{LayerDigest: "sha256:new", ULength: 7}}}}})
	for _, n := range []*common.ClipNode{{Path: "/directory", NodeType: common.DirNode, Attr: fuse.Attr{Ino: 999, Nlink: 17}}, {Path: "/symlink", NodeType: common.SymLinkNode, Attr: fuse.Attr{Ino: 999, Nlink: 3}}, {Path: "/zero", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 0, Nlink: 4}}} {
		idx.Set(n)
	}
	normalizeFileLinkCounts(idx, nil)
	assertLinkGroup(t, ca, idx, []string{"/alias", "/third"}, 2)
	assertLinkGroup(t, ca, idx, []string{"/public"}, 1)
	for path, want := range map[string]uint32{"/directory": 17, "/symlink": 3, "/zero": 4} {
		if got := idx.Get(&common.ClipNode{Path: path}).(*common.ClipNode).Attr.Nlink; got != want {
			t.Fatal(fmt.Sprintf("non-group metadata changed: %s %d", path, got))
		}
	}
	normalizeFileLinkCounts(ca.newIndex(), nil)
}
