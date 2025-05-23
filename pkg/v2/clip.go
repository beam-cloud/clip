package clipv2

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/ristretto"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/moby/sys/mountinfo"
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
	ContentCache              ContentCache
	ContentCacheAvailable     bool
	MountPoint                string
	CacheLocally              bool
	WarmChunks                bool
	PriorityChunks            []string
	PriorityChunkSampleTime   time.Duration
	SetPriorityChunksCallback func(chunks []string) error
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

	if options.WarmChunks && options.ContentCache != nil && len(options.PriorityChunks) > 0 {
		go func() {
			err := options.ContentCache.WarmChunks(options.PriorityChunks, "https://beam-cdn.com"+"/"+options.ImageID+"/chunks")
			if err != nil {
				log.Error().Err(err).Msg("failed to warm chunks")
			}
		}()
	}

	chunkCache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1e7,
		MaxCost:     1 * 1e9,
		BufferItems: 64,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not create local cache: %v", err)
	}

	var priorityChunkCallback func(chunks []string) error = nil
	if options.SetPriorityChunksCallback != nil {
		// Only set the callback if there is not already a list of priority chunks
		log.Info().Msg("Setting priority chunks callback")
		priorityChunkCallback = options.SetPriorityChunksCallback
	}

	storage, err := NewClipStorage(ClipStorageOpts{
		ImageID:                  options.ImageID,
		ArchivePath:              options.LocalPath,
		ChunkPath:                options.OutputPath,
		Metadata:                 metadata,
		ContentCache:             options.ContentCache,
		ChunkCache:               chunkCache,
		SetPriorityChunkCallback: priorityChunkCallback,
		PriorityChunkSampleTime:  options.PriorityChunkSampleTime,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not load storage: %v", err)
	}

	clipfs, err := NewFileSystem(storage, chunkCache, ClipFileSystemOpts{Verbose: options.Verbose, ContentCache: options.ContentCache, ContentCacheAvailable: options.ContentCacheAvailable})
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
		MaxReadAhead:         1024 * 1024 * 32, // 32MB
		MaxWrite:             1024 * 1024,      // 1MB
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

			err = updateReadAheadKB(options.MountPoint, 1024*1024*32)
			if err != nil {
				log.Error().Err(err).Msg("failed to update read_ahead_kb")
			}

			server.Wait()
			storage.Cleanup()

			close(serverError)
		}()

		return nil
	}

	return startServer, serverError, server, nil
}

func updateReadAheadKB(mountPoint string, valueKB int) error {
	mounts, err := mountinfo.GetMounts(nil)
	if err != nil {
		return fmt.Errorf("failed to get mount info: %w", err)
	}

	var deviceID string
	for _, mount := range mounts {
		if mount.Mountpoint == mountPoint {
			deviceID = fmt.Sprintf("%d:%d", mount.Major, mount.Minor)
			break
		}
	}

	if deviceID == "" {
		return fmt.Errorf("mount point %s not found", mountPoint)
	}

	// Construct path to read_ahead_kb
	readAheadPath := fmt.Sprintf("/sys/class/bdi/%s/read_ahead_kb", deviceID)

	// Update read_ahead_kb
	cmd := exec.Command("sh", "-c", fmt.Sprintf("echo %d > %s", valueKB, readAheadPath))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to update read_ahead_kb: %w read_ahead_path: %s", err, readAheadPath)
	}

	log.Info().Msgf("updated read_ahead_kb to %d", valueKB)

	return nil
}
