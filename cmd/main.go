package main

import (
	// clipfs "github.com/beam-cloud/clip/pkg/fs"
	"log"

	archive "github.com/beam-cloud/clip/pkg/archive"
)

func main() {
	fs := archive.NewFileSystem()

	nodes := []*archive.FsNode{
		{NodeType: archive.DirNode, Path: "/dir1"},
		{NodeType: archive.DirNode, Path: "/dir2"},
		{NodeType: archive.FileNode, Path: "/dir1/file1", Size: 100},
		{NodeType: archive.SymLinkNode, Path: "/dir2/link1", Target: "/dir1/file1"},
	}

	for _, n := range nodes {
		fs.Insert(n)
	}

	if err := fs.DumpToFile("filesystem.gob"); err != nil {
		log.Fatal(err)
	}

	// fs2 := archive.NewFileSystem()
	// if err := fs2.LoadFromFile("filesystem.gob"); err != nil {
	// 	log.Fatal(err)
	// }

	// fs2.PrintNodes()

	// c, err := fuse.Mount(
	// 	"/tmp/test",
	// 	fuse.FSName("helloworld"),
	// 	fuse.Subtype("hellofs"),
	// )

	// if err != nil {
	// 	log.Fatal(err)
	// }
	// defer c.Close()

	// err = fs.Serve(c, clipfs.NewFS())
	// if err != nil {
	// 	log.Fatal(err)
	// }
}
