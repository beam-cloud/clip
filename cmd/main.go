package main

import (
	"log"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	archive "github.com/beam-cloud/clip/pkg/archive"
	clipfs "github.com/beam-cloud/clip/pkg/fs"
)

func main() {
	archiver, err := archive.NewClipArchiver()
	if err != nil {
		return
	}

	cf, err := archiver.CreateFromDirectory("/images/748973e7feb2c29f")
	if err != nil {
		log.Fatalf("unable to create archive: %v", err)
	}

	log.Printf("created new clip: <%+v>", cf)

	// cfs.PrintNodes()

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
