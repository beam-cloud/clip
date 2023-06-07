package main

import (
	"log"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	archive "github.com/beam-cloud/clip/pkg/archive"
	mount "github.com/beam-cloud/clip/pkg/mount"
)

func main() {
	archiver, err := archive.NewClipArchiver()
	if err != nil {
		return
	}

	cfs, err := archiver.CreateFromDirectory("/images/748973e7feb2c29f")
	if err != nil {
		log.Fatalf("unable to create archive: %v", err)
	}

	log.Printf("created new clipfs: <%+v>", cfs)

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

	err = fs.Serve(c, mount.NewFS())
	if err != nil {
		log.Fatal(err)
	}
}
