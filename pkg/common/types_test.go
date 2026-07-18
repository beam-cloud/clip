package common

import (
	"fmt"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/btree"
)

func newTestMetadata(nodes ...*ClipNode) *ClipArchiveMetadata {
	index := btree.New(func(a, b interface{}) bool {
		return a.(*ClipNode).Path < b.(*ClipNode).Path
	})
	metadata := &ClipArchiveMetadata{Index: index}
	for _, node := range nodes {
		metadata.Insert(node)
	}
	return metadata
}

func TestListDirectoryReturnsImmediateChildren(t *testing.T) {
	metadata := newTestMetadata(
		&ClipNode{Path: "/target/alpha", Attr: fuse.Attr{Mode: 0644}},
		&ClipNode{Path: "/target/beta", Attr: fuse.Attr{Mode: 0755}},
		&ClipNode{Path: "/target/nested/child", Attr: fuse.Attr{Mode: 0644}},
		&ClipNode{Path: "/targeted/unrelated", Attr: fuse.Attr{Mode: 0644}},
		&ClipNode{Path: "/z/unrelated", Attr: fuse.Attr{Mode: 0644}},
	)

	require.Equal(t, []fuse.DirEntry{
		{Mode: 0644, Name: "alpha"},
		{Mode: 0755, Name: "beta"},
	}, metadata.ListDirectory("/target"))
}

func BenchmarkListDirectoryLargeImage(b *testing.B) {
	metadata := newTestMetadata()
	for i := 0; i < 64; i++ {
		metadata.Insert(&ClipNode{Path: fmt.Sprintf("/target/file-%03d", i)})
	}
	for i := 0; i < 100_000; i++ {
		metadata.Insert(&ClipNode{Path: fmt.Sprintf("/z/file-%06d", i)})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries := metadata.ListDirectory("/target")
		if len(entries) != 64 {
			b.Fatalf("got %d entries, want 64", len(entries))
		}
	}
}
