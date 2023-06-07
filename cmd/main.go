package main

import (
	"log"
	"time"

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

	start := time.Now()
	ca, err := archiver.Create("/images/748973e7feb2c29f", "test.clip")
	if err != nil {
		log.Fatalf("unable to create archive: %v", err)
	}

	// ca.PrintNodes()

	log.Println("Archived image, took:", time.Since(start))
	log.Printf("created new clip: <%+v>", ca)

	val := ca.Get("/rootfs/var/log/dpkg.log")
	log.Printf("v: %+v", val)

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
