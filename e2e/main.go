package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
	clipv2 "github.com/beam-cloud/clip/pkg/v2"
	"github.com/beam-cloud/clip/pkg/v2/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog/log"
)

func main() {
	// local()
	s3()
}

func s3() {
	// Get access key, secret key, bucket, region, endpoint from env vars
	accessKey := os.Getenv("WS_ACCESS_KEY")
	secretKey := os.Getenv("WS_SECRET_KEY")
	bucket := os.Getenv("WS_BUCKET")
	region := os.Getenv("WS_REGION")
	endpoint := os.Getenv("WS_ENDPOINT")

	log.Info().Str("accessKey", accessKey).Str("secretKey", secretKey).Str("bucket", bucket).Str("region", region).Str("endpoint", endpoint).Msg("S3 credentials")

	createOptions := clipv2.CreateOptions{
		IndexID:    "1234567890",
		SourcePath: "../test",
		LocalPath:  "",
		S3Config: common.S3StorageInfo{
			Bucket:    bucket,
			Region:    region,
			AccessKey: accessKey,
			SecretKey: secretKey,
			Endpoint:  endpoint,
		},
		Verbose:      false,
		MaxChunkSize: 0,
		StorageMode:  clipv2.StorageModeS3,
	}

	extractOptions := clipv2.ExtractOptions{
		IndexID:     "1234567890",
		LocalPath:   "",
		StorageMode: clipv2.StorageModeS3,
		Verbose:     false,
		S3Config: common.S3StorageInfo{
			Bucket:    bucket,
			Region:    region,
			AccessKey: accessKey,
			SecretKey: secretKey,
			Endpoint:  endpoint,
		},
	}

	startTime := time.Now()
	err := clipv2.CreateArchive(createOptions)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create archive")
		os.Exit(1)
	}
	duration := time.Since(startTime)
	log.Info().Msgf("Time taken to create archive: %v", duration)

	metadata, err := clipv2.ExtractMetadata(extractOptions)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get metadata")
		os.Exit(1)
	}

	header := metadata.Header
	index := metadata.Index
	chunks := metadata.Chunks

	log.Info().Msgf("Metadata: %+v\n", header)
	log.Info().Msgf("Tree: %+v\n", index)
	log.Info().Msgf("ChunkHashes: %+v\n", chunks)

	cdnStorage := storage.NewCDNClipStorage("https://beam-cdn.com", "1234567890", metadata)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create CDN clip storage")
		os.Exit(1)
	}

	fsOpts := clip.ClipFileSystemOpts{
		Verbose:               true,
		ContentCache:          nil,
		ContentCacheAvailable: false,
	}

	clipFileSystem, err := clip.NewFileSystem(cdnStorage, fsOpts)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create clip file system")
		os.Exit(1)
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
		log.Error().Err(err).Msg("Failed to create fuse server")
		os.Exit(1)
	}
	server.Serve()
}

func local() {
	createOptions := clipv2.CreateOptions{
		SourcePath:  "../test",
		LocalPath:   "/tmp/clip-archives",
		IndexID:     "1234567890",
		StorageMode: clipv2.StorageModeLocal,
	}

	extractOptions := clipv2.ExtractOptions{
		IndexID:     "1234567890",
		LocalPath:   "/tmp/clip-archives",
		StorageMode: clipv2.StorageModeLocal,
	}

	err := clipv2.CreateArchive(createOptions)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create archive")
		os.Exit(1)
	}

	metadata, err := clipv2.ExtractMetadata(extractOptions)
	if err != nil {
		log.Error().Err(err).Msg("Failed to extract metadata")
		os.Exit(1)
	}

	header := metadata.Header
	index := metadata.Index
	chunkHashes := metadata.Chunks

	log.Info().Msgf("Metadata: %+v\n", header)
	log.Info().Msgf("Tree: %+v\n", index)
	log.Info().Msgf("ChunkHashes: %+v\n", chunkHashes)
	localStorage, err := storage.NewLocalClipStorage(metadata, storage.LocalClipStorageOpts{
		ArchivePath: filepath.Join(createOptions.LocalPath, "index.clip"),
		ChunkDir:    filepath.Join(createOptions.LocalPath, "chunks"),
	})
	if err != nil {
		log.Error().Err(err).Msg("Failed to create local clip storage")
		os.Exit(1)
	}

	fsOpts := clip.ClipFileSystemOpts{
		Verbose:               true,
		ContentCache:          nil,
		ContentCacheAvailable: false,
	}

	clipFileSystem, err := clip.NewFileSystem(localStorage, fsOpts)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create clip file system")
		os.Exit(1)
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
		log.Error().Err(err).Msg("Failed to create fuse server")
		os.Exit(1)
	}
	server.Serve()
}
