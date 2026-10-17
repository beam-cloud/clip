package common

import (
	"fmt"
	"math/rand"
	"path"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestMetadataListDirectoryTraversal(t *testing.T) {
	metadata := newTestMetadata()
	nodes := []*ClipNode{
		{Path: "/", Attr: fuse.Attr{Mode: syscall.S_IFDIR | 0755}},
		{Path: "/target", Attr: fuse.Attr{Mode: syscall.S_IFDIR | 0700}},
		{Path: "/target/a", Attr: fuse.Attr{Mode: syscall.S_IFDIR | 0750}},
		{Path: "/target/a/deep/child", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0600}},
		{Path: "/target/a.", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0640}},
		{Path: "/target/a0", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0644}},
		{Path: "/target/a0/child", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0644}},
		{Path: "/target/a\xff", Attr: fuse.Attr{Mode: syscall.S_IFLNK | 0777}},
		{Path: "/target/link", Attr: fuse.Attr{Ino: 99, Mode: syscall.S_IFLNK | 0777}, Target: "a"},
		{Path: "/target/hard", Attr: fuse.Attr{Ino: 100, Mode: syscall.S_IFREG | 0400}},
		{Path: "/target/é", Attr: fuse.Attr{Mode: syscall.S_IFDIR | 01770}},
		{Path: "/target/é/child", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0600}},
		{Path: "/target/未", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0000}},
		{Path: "/targeted/other", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0644}},
		{Path: "/z", Attr: fuse.Attr{Mode: syscall.S_IFREG | 0644}},
	}
	for _, node := range nodes {
		metadata.Insert(node)
	}
	for _, directory := range []string{"/", "/target", "/target/", "/target/a", "/target/a/", "/missing", "/target/a0"} {
		t.Run(directory, func(t *testing.T) { assertDirectoryMatchesModel(t, metadata, directory) })
	}
	// Directory enumeration must not follow the symlink or alter hardlink metadata.
	if metadata.Get("/target/link").Target != "a" || metadata.Get("/target/hard").Attr.Mode != syscall.S_IFREG|0400 {
		t.Fatal("listing changed metadata")
	}
}

func TestMetadataListDirectoryGeneratedTrees(t *testing.T) {
	rng := rand.New(rand.NewSource(8461))
	metadata := newTestMetadata()
	directories := map[string]bool{"/": true}
	names := []string{"a", "a.", "a0", "dir", "with space", "quote'", "é", "未", "\x01", "\xff"}
	for i := 0; i < 10_000; i++ {
		parts := []string{"root"}
		for j := 0; j < rng.Intn(7); j++ {
			parts = append(parts, names[rng.Intn(len(names))])
		}
		for j := 1; j <= len(parts); j++ {
			directories["/"+strings.Join(parts[:j], "/")] = true
		}
		metadata.Insert(&ClipNode{Path: "/" + strings.Join(parts, "/") + fmt.Sprintf("/file-%06d", i), Attr: fuse.Attr{Mode: syscall.S_IFREG | uint32(rng.Intn(4096))}})
	}
	// Include explicit directories in a shuffled order, as layered indexes do.
	all := make([]string, 0, len(directories))
	for directory := range directories {
		all = append(all, directory)
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	for _, directory := range all {
		metadata.Insert(&ClipNode{Path: directory, Attr: fuse.Attr{Mode: syscall.S_IFDIR | 0750}})
	}
	model := make(map[string][]fuse.DirEntry)
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*ClipNode)
		if node.Path != "/" {
			parent := path.Dir(node.Path)
			model[parent] = append(model[parent], fuse.DirEntry{Name: path.Base(node.Path), Mode: node.Attr.Mode, Ino: node.Attr.Ino})
		}
		return true
	})
	for _, directory := range all {
		want := model[directory]
		sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
		assertDirectoryEntries(t, directory, metadata.ListDirectory(directory), want)
	}
}

func assertDirectoryMatchesModel(t *testing.T, metadata *ClipArchiveMetadata, directory string) {
	t.Helper()
	clean := strings.TrimSuffix(directory, "/")
	if clean == "" {
		clean = "/"
	}
	var want []fuse.DirEntry
	// Independent model: select records whose path.Dir is the requested parent.
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node := item.(*ClipNode)
		if node.Path != "/" && path.Dir(node.Path) == clean {
			want = append(want, fuse.DirEntry{Name: path.Base(node.Path), Mode: node.Attr.Mode, Ino: node.Attr.Ino})
		}
		return true
	})
	sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
	assertDirectoryEntries(t, directory, metadata.ListDirectory(directory), want)
}

func assertDirectoryEntries(t *testing.T, directory string, got, want []fuse.DirEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%q: got%d entries, want%d", directory, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q entry%d: got%+v want%+v", directory, i, got[i], want[i])
		}
	}
}
