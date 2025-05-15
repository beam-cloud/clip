package clipv2

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
)

type ClipV2 struct {
	Metadata *common.ClipArchiveMetadata
}

type CreateOptions struct {
	ImageID      string
	SourcePath   string
	LocalPath    string
	S3Config     common.S3StorageInfo
	StorageType  common.StorageMode
	MaxChunkSize int64
	Verbose      bool
	ProgressChan chan<- int
}

type ExtractOptions struct {
	ImageID     string
	LocalPath   string
	OutputPath  string
	S3Config    common.S3StorageInfo
	StorageType common.StorageMode
	Verbose     bool
}

type MountOptions struct {
	ExtractOptions
	ContentCache          ContentCache
	ContentCacheAvailable bool
	MountPoint            string
	CacheLocally          bool
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	a := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:      options.ImageID,
		SourcePath:   options.SourcePath,
		LocalPath:    options.LocalPath,
		StorageType:  options.StorageType,
		S3Config:     options.S3Config,
		MaxChunkSize: options.MaxChunkSize,
		Verbose:      options.Verbose,
	})
	return a.Create()
}

func ExtractMetadata(options ExtractOptions) (*ClipV2Archive, error) {
	a := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:     options.ImageID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageType: options.StorageType,
		S3Config:    options.S3Config,
		Verbose:     options.Verbose,
	})
	return a.ExtractArchive(context.Background())
}

func ExpandLocalArchive(ctx context.Context, options ExtractOptions) error {
	a := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:     options.ImageID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageType: options.StorageType,
		S3Config:    options.S3Config,
		Verbose:     options.Verbose,
	})
	return a.ExpandLocalArchive(ctx)
}

// Mount a clip archive to a directory
func MountArchive(ctx context.Context, options MountOptions) (func() error, <-chan error, *fuse.Server, error) {
	log.Info().Msgf("Mounting archive %s to %s", options.ImageID, options.MountPoint)

	if _, err := os.Stat(options.MountPoint); os.IsNotExist(err) {
		err = os.MkdirAll(options.MountPoint, 0755)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to create mount point directory: %v", err)
		}
	}

	ca := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:     options.ImageID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageType: options.StorageType,
		S3Config:    options.S3Config,
		Verbose:     options.Verbose,
	})
	metadata, err := ca.ExtractArchive(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid archive: %v", err)
	}

	storage, err := NewClipStorage(ClipStorageOpts{
		ImageID:      options.ImageID,
		ArchivePath:  options.LocalPath,
		ChunkPath:    options.OutputPath,
		Metadata:     metadata,
		ContentCache: options.ContentCache,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not load storage: %v", err)
	}

	clipfs, err := NewFileSystem(storage, ClipFileSystemOpts{Verbose: options.Verbose, ContentCache: options.ContentCache, ContentCacheAvailable: options.ContentCacheAvailable})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not create filesystem: %v", err)
	}

	root, _ := clipfs.Root()
	attrTimeout := time.Second * 60
	entryTimeout := time.Second * 60
	fsOptions := &fs.Options{
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
	}
	server, err := fuse.NewServer(fs.NewNodeFS(root, fsOptions), options.MountPoint, &fuse.MountOptions{
		MaxBackground:        512,
		DisableXAttrs:        true,
		EnableSymlinkCaching: true,
		SyncRead:             false,
		RememberInodes:       true,
		MaxReadAhead:         1024 * 128, // 128KB
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not create server: %v", err)
	}

	serverError := make(chan error, 1)
	startServer := func() error {
		go func() {
			go server.Serve()

			if err := server.WaitMount(); err != nil {
				serverError <- err
				return
			}

			server.Wait()
			storage.Cleanup()

			close(serverError)
		}()

		return nil
	}

	return startServer, serverError, server, nil
}
