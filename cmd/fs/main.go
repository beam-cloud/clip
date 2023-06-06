package main

import (
	"log"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	clipfs "github.com/beam-cloud/clip/pkg/fs"
)

func main() {
	c, err := fuse.Mount(
		"/tmp/test",
		fuse.FSName("helloworld"),
		fuse.Subtype("hellofs"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	err = fs.Serve(c, clipfs.NewFS())
	if err != nil {
		log.Fatal(err)
	}
}
