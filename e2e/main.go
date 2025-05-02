package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/beam-cloud/clip/pkg/clip"
	clipv2 "github.com/beam-cloud/clip/pkg/v2"
	"github.com/beam-cloud/clip/pkg/v2/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	clipArchiver := clipv2.NewClipV2Archiver()

	opts := clipv2.ClipV2ArchiverOptions{
		SourcePath: "../test",
		ChunkDir:   "/tmp/cblocks",
		IndexPath:  "/tmp/test.clip",
	}
	cwd, _ := os.Getwd()
	fmt.Println("cwd", cwd)

	err := clipArchiver.Create(opts)
	if err != nil {
		log.Fatalf("Failed to create archive: %v", err)
	}

	metadata, err := clipArchiver.ExtractArchive(opts.IndexPath)
	if err != nil {
		log.Fatalf("Failed to extract metadata: %v", err)
	}

	header := metadata.Header
	index := metadata.Index
	chunkHashes := metadata.ChunkHashes

	fmt.Printf("Metadata: %+v\n", header)
	fmt.Printf("Tree: %+v\n", index)
	fmt.Printf("ChunkHashes: %+v\n", chunkHashes)
	localStorage, err := storage.NewLocalClipStorage(metadata, storage.LocalClipStorageOpts{
		ArchivePath: opts.IndexPath,
		ChunkDir:    opts.ChunkDir,
	})
	if err != nil {
		log.Fatalf("Failed to create local clip storage: %v", err)
	}

	fsOpts := clip.ClipFileSystemOpts{
		Verbose:               true,
		ContentCache:          nil,
		ContentCacheAvailable: false,
	}

	clipFileSystem, err := clip.NewFileSystem(localStorage, fsOpts)
	if err != nil {
		log.Fatalf("Failed to create clip file system: %v", err)
	}

	root, _ := clipFileSystem.Root()
	attrTimeout := time.Second * 60
	entryTimeout := time.Second * 60
	fsOptions := &fs.Options{
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
	}
	server, err := fuse.NewServer(fs.NewNodeFS(root, fsOptions), "/tmp/clipfs", &fuse.MountOptions{
		MaxBackground:        512,
		DisableXAttrs:        true,
		EnableSymlinkCaching: true,
		SyncRead:             false,
		RememberInodes:       true,
		MaxReadAhead:         1024 * 128, // 128KB
	})
	if err != nil {
		log.Fatalf("could not create server: %v", err)
	}
	server.Serve()
}
