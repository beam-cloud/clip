package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
	clipv2 "github.com/beam-cloud/clip/pkg/v2"
	"github.com/beam-cloud/clip/pkg/v2/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	// local()
	s3()
}

func s3() {
	clipArchiver := clipv2.NewClipV2Archiver()
	// Get access key, secret key, bucket, region, endpoint from env vars
	accessKey := os.Getenv("WS_ACCESS_KEY")
	secretKey := os.Getenv("WS_SECRET_KEY")
	bucket := os.Getenv("WS_BUCKET")
	region := os.Getenv("WS_REGION")
	endpoint := os.Getenv("WS_ENDPOINT")

	opts := clipv2.ClipV2ArchiverOptions{
		SourcePath: "../test",
		LocalPath:  "/tmp/clip-archives",
		IndexID:    "1234567890",
		S3Config: common.S3StorageInfo{
			Bucket:    bucket,
			Region:    region,
			AccessKey: accessKey,
			SecretKey: secretKey,
			Endpoint:  endpoint,
		},
		Verbose:      false,
		Compress:     false,
		OutputPath:   "",
		MaxChunkSize: 0,
		Destination:  clipv2.DestinationTypeS3,
	}

	startTime := time.Now()
	err := clipArchiver.Create(opts)
	if err != nil {
		log.Fatalf("Failed to create archive: %v", err)
	}
	duration := time.Since(startTime)
	fmt.Printf("Time taken to create archive: %v\n", duration)

	metadata, err := clipArchiver.ExtractArchive(context.Background(), opts)
	if err != nil {
		log.Fatalf("Failed to extract metadata: %v", err)
	}

	header := metadata.Header
	index := metadata.Index
	chunks := metadata.Chunks

	fmt.Printf("Metadata: %+v\n", header)
	fmt.Printf("Tree: %+v\n", index)
	fmt.Printf("ChunkHashes: %+v\n", chunks)

	cdnStorage := storage.NewCDNClipStorage("https://beam-cdn.com", "1234567890", metadata)
	if err != nil {
		log.Fatalf("Failed to create CDN clip storage: %v", err)
	}

	fsOpts := clip.ClipFileSystemOpts{
		Verbose:               true,
		ContentCache:          nil,
		ContentCacheAvailable: false,
	}

	clipFileSystem, err := clip.NewFileSystem(cdnStorage, fsOpts)
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

func local() {
	clipArchiver := clipv2.NewClipV2Archiver()

	opts := clipv2.ClipV2ArchiverOptions{
		SourcePath: "../test",
		LocalPath:  "/tmp/clip-archives",
	}
	cwd, _ := os.Getwd()
	fmt.Println("cwd", cwd)

	err := clipArchiver.Create(opts)
	if err != nil {
		log.Fatalf("Failed to create archive: %v", err)
	}

	metadata, err := clipArchiver.ExtractArchive(context.Background(), opts)
	if err != nil {
		log.Fatalf("Failed to extract metadata: %v", err)
	}

	header := metadata.Header
	index := metadata.Index
	chunkHashes := metadata.Chunks

	fmt.Printf("Metadata: %+v\n", header)
	fmt.Printf("Tree: %+v\n", index)
	fmt.Printf("ChunkHashes: %+v\n", chunkHashes)
	localStorage, err := storage.NewLocalClipStorage(metadata, storage.LocalClipStorageOpts{
		ArchivePath: filepath.Join(opts.LocalPath, "index.clip"),
		ChunkDir:    filepath.Join(opts.LocalPath, "chunks"),
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
