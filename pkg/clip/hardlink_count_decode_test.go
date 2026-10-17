package clip

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fuse"
	"os"
	"path/filepath"
	"testing"
)

func oldCountArchive(t testing.TB, ca *ClipArchiver, nodes []*common.ClipNode, version uint8) string {
	t.Helper()
	var index []byte
	var err error
	if version == 1 {
		var b bytes.Buffer
		err = gob.NewEncoder(&b).Encode(nodes)
		index = b.Bytes()
	} else {
		idx := ca.newIndex()
		for _, n := range nodes {
			idx.Set(n)
		}
		sum := sha256.Sum256([]byte("synthetic-original"))
		index, err = ca.encodeIndexV2(idx, sum[:], common.ClipHeaderLength)
	}
	if err != nil {
		t.Fatal(err)
	}
	h := &common.ClipArchiveHeader{ClipFileFormatVersion: version, IndexPos: common.ClipHeaderLength, IndexLength: int64(len(index))}
	copy(h.StartBytes[:], common.ClipFileStartBytes)
	storage, err := (common.OCIStorageInfo{}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	var storageBuf bytes.Buffer
	if err := gob.NewEncoder(&storageBuf).Encode(common.StorageInfoWrapper{Type: "oci", Data: storage}); err != nil {
		t.Fatal(err)
	}
	h.StorageInfoPos = common.ClipHeaderLength + int64(len(index))
	h.StorageInfoLength = int64(storageBuf.Len())
	header, err := ca.EncodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic.clip")
	if err := os.WriteFile(path, append(append(header, index...), storageBuf.Bytes()...), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestExistingArchiveHardLinkCounts(t *testing.T) {
	for _, version := range []uint8{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ca := NewClipArchiver()
			nodes := []*common.ClipNode{
				{Path: "/alias1", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 17, Nlink: 1, Mode: 0100640, Owner: fuse.Owner{Uid: 1001, Gid: 1002}}, Remote: &common.RemoteRef{LayerDigest: "sha256:payload", UOffset: 512, ULength: 7}},
				{Path: "/alias2", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 17, Nlink: 1, Mode: 0100640, Owner: fuse.Owner{Uid: 1001, Gid: 1002}}, Remote: &common.RemoteRef{LayerDigest: "sha256:payload", UOffset: 512, ULength: 7}},
				{Path: "/alias3", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 17, Nlink: 1, Mode: 0100640, Owner: fuse.Owner{Uid: 1001, Gid: 1002}}, Remote: &common.RemoteRef{LayerDigest: "sha256:payload", UOffset: 512, ULength: 7}},
			}
			path := oldCountArchive(t, ca, nodes, version)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := ca.ExtractMetadata(path)
			if err != nil {
				t.Fatal(err)
			}
			assertLinkGroup(t, ca, metadata.Index, []string{"/alias1", "/alias2", "/alias3"}, 3)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("archive bytes changed")
			}
			for _, p := range []string{"/alias1", "/alias2", "/alias3"} {
				n := metadata.Get(p)
				if n.Attr.Mode != 0100640 || n.Attr.Uid != 1001 || n.Attr.Gid != 1002 || n.Remote == nil || n.Remote.UOffset != 512 || n.Remote.ULength != 7 {
					t.Fatal("other metadata changed")
				}
			}
		})
	}
}
func TestExistingArchiveDuplicatePathsCountFinalVisibleNodes(t *testing.T) {
	ca := NewClipArchiver()
	nodes := []*common.ClipNode{{Path: "/alias", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 17, Nlink: 99}, Remote: &common.RemoteRef{LayerDigest: "sha256:old"}}, {Path: "/target", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 17, Nlink: 99}, Remote: &common.RemoteRef{LayerDigest: "sha256:old"}}, {Path: "/target", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 18, Nlink: 99}, Remote: &common.RemoteRef{LayerDigest: "sha256:new"}}}
	metadata, err := ca.ExtractMetadata(oldCountArchive(t, ca, nodes, 1))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Get("/alias").Attr.Nlink != 1 || metadata.Get("/target").Attr.Nlink != 1 {
		t.Fatal("replaced decoded node counted as visible")
	}
}

func TestNativeArchiveLinkMetadataUnchanged(t *testing.T) {
	ca := NewClipArchiver()
	nodes := []*common.ClipNode{{Path: "/native1", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 1, Nlink: 2}}, {Path: "/native2", NodeType: common.FileNode, Attr: fuse.Attr{Ino: 2, Nlink: 2}}}
	for _, version := range []uint8{1, 2} {
		for _, kind := range []string{"local", "s3", "oci"} {
			t.Run(fmt.Sprintf("v%d/%s", version, kind), func(t *testing.T) {
				path := oldCountArchive(t, ca, nodes, version)
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				header, err := ca.DecodeHeader(original[:common.ClipHeaderLength])
				if err != nil {
					t.Fatal(err)
				}
				body := original[common.ClipHeaderLength:header.StorageInfoPos]
				if kind == "local" {
					header.StorageInfoLength = 0
					header.StorageInfoPos = 0
				} else if kind == "s3" {
					raw, err := (common.S3StorageInfo{}).Encode()
					if err != nil {
						t.Fatal(err)
					}
					var encoded bytes.Buffer
					if err := gob.NewEncoder(&encoded).Encode(common.StorageInfoWrapper{Type: "s3", Data: raw}); err != nil {
						t.Fatal(err)
					}
					header.StorageInfoLength = int64(encoded.Len())
					body = append(body, encoded.Bytes()...)
				} else {
					body = original[common.ClipHeaderLength:]
				}
				hb, err := ca.EncodeHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(hb, body...), 0600); err != nil {
					t.Fatal(err)
				}
				m, err := ca.ExtractMetadata(path)
				if err != nil {
					t.Fatal(err)
				}
				if m.Get("/native1").Attr.Nlink != 2 || m.Get("/native2").Attr.Nlink != 2 {
					t.Fatal("native attribute lost")
				}
			})
		}
	}
}
