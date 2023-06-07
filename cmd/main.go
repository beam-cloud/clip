package main

import (
	"log"
	"time"

	archive "github.com/beam-cloud/clip/pkg/archive"
)

func main() {
	fs := archive.NewFileSystem()
	log.Println("archiving")
	start := time.Now()
	archive.PopulateFromDirectory(fs, "/images/748973e7feb2c29f")

	duration := time.Since(start)
	log.Printf("done, took: %v", duration)

	if err := fs.DumpToFile("filesystem.gob"); err != nil {
		log.Fatal(err)
	}

	log.Println("loading fs from disk")
	start = time.Now()
	fs2 := archive.NewFileSystem()
	if err := fs2.LoadFromFile("filesystem.gob"); err != nil {
		log.Fatal(err)
	}
	duration = time.Since(start)
	log.Printf("done, took: %v", duration)

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
