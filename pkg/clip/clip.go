package clip

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// SetLogLevel configures the logging verbosity for the CLIP library.
// Valid levels: "debug", "info", "warn", "error", "disabled"
// Use "debug" to see detailed operation logs (file operations, cache hits/misses, etc.)
// Use "info" for high-level operation logs (default)
// Use "disabled" to suppress all logs
func SetLogLevel(level string) error {
	switch strings.ToLower(level) {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn", "warning":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case "disabled", "none", "off":
		zerolog.SetGlobalLevel(zerolog.Disabled)
	default:
		return fmt.Errorf("invalid log level %q: must be one of: debug, info, warn, error, disabled", level)
	}
	return nil
}

type CreateOptions struct {
	InputPath    string
	OutputPath   string
	Credentials  storage.ClipStorageCredentials
	ProgressChan chan<- int
}

type CreateRemoteOptions struct {
	InputPath  string
	OutputPath string
}

type ExtractOptions struct {
	InputFile  string
	OutputPath string
}

type MountOptions struct {
	ArchivePath           string
	MountPoint            string
	CachePath             string
	ContentCache          storage.ContentCache
	ContentCacheAvailable bool
	StorageInfo           common.ClipStorageInfo
	Credentials           storage.ClipStorageCredentials
}

type StoreS3Options struct {
	ArchivePath  string
	OutputFile   string
	Bucket       string
	Key          string
	CachePath    string
	Credentials  storage.ClipStorageCredentials
	ProgressChan chan<- int
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	log.Info().Msgf("creating archive from %s to %s", options.InputPath, options.OutputPath)

	a := NewClipArchiver()
	err := a.Create(ClipArchiverOptions{
		SourcePath: options.InputPath,
		OutputFile: options.OutputPath,
	})
	if err != nil {
		return err
	}

	log.Info().Msg("archive created successfully")
	return nil
}

func CreateAndUploadArchive(ctx context.Context, options CreateOptions, si common.ClipStorageInfo) error {
	log.Info().Msgf("creating archive from %s to %s", options.InputPath, options.OutputPath)

	// Create a temporary file for storing the clip
	tempFile, err := os.CreateTemp("", "temp-clip-*.clip")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name()) // Cleanup the temporary clip (after upload it is stored remotely)

	localArchiver := NewClipArchiver()
	err = localArchiver.Create(ClipArchiverOptions{
		SourcePath: options.InputPath,
		OutputFile: tempFile.Name(),
	})
	if err != nil {
		return err
	}

	remoteArchiver, err := NewRClipArchiver(si)
	if err != nil {
		return err
	}

	err = remoteArchiver.Create(ctx, tempFile.Name(), options.OutputPath, options.Credentials, options.ProgressChan)
	if err != nil {
		return err
	}

	log.Info().Msg("archive created successfully")
	return nil
}

// Extract Archive
func ExtractArchive(options ExtractOptions) error {
	log.Info().Msgf("extracting archive: %s", options.InputFile)

	a := NewClipArchiver()
	err := a.Extract(ClipArchiverOptions{
		ArchivePath: options.InputFile,
		OutputPath:  options.OutputPath,
	})

	if err != nil {
		return err
	}

	log.Info().Msg("archive extracted successfully")
	return nil
}

// Mount a clip archive to a directory
func MountArchive(options MountOptions) (func() error, <-chan error, *fuse.Server, error) {
	log.Info().Msgf("mounting archive %s to %s", options.ArchivePath, options.MountPoint)

	if _, err := os.Stat(options.MountPoint); os.IsNotExist(err) {
		err = os.MkdirAll(options.MountPoint, 0755)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to create mount point directory: %v", err)
		}
	}

	ca := NewClipArchiver()
	metadata, err := ca.ExtractMetadata(options.ArchivePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid archive: %v", err)
	}

	// Handle StorageInfo type conversion
	var s3Info *common.S3StorageInfo
	if options.StorageInfo != nil {
		if si, ok := options.StorageInfo.(*common.S3StorageInfo); ok {
			s3Info = si
		} else if si, ok := options.StorageInfo.(common.S3StorageInfo); ok {
			s3Info = &si
		}
	}

	storage, err := storage.NewClipStorage(storage.ClipStorageOpts{
		ArchivePath: options.ArchivePath,
		CachePath:   options.CachePath,
		Metadata:    metadata,
		Credentials: options.Credentials,
		StorageInfo: s3Info,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not load storage: %v", err)
	}

	clipfs, err := NewFileSystem(storage, ClipFileSystemOpts{ContentCache: options.ContentCache, ContentCacheAvailable: options.ContentCacheAvailable})
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

// Store CLIP in remote storage
func StoreS3(storeS3Opts StoreS3Options) error {
	log.Info().Msg("uploading archive")

	region := os.Getenv("AWS_REGION")

	// If no key is provided, use the base name of the input archive as key
	if storeS3Opts.Key == "" {
		storeS3Opts.Key = filepath.Base(storeS3Opts.ArchivePath)
	}

	storageInfo := &common.S3StorageInfo{Bucket: storeS3Opts.Bucket, Key: storeS3Opts.Key, Region: region}
	a, err := NewRClipArchiver(storageInfo)
	if err != nil {
		return err
	}

	err = a.Create(context.Background(), storeS3Opts.ArchivePath, storeS3Opts.OutputFile, storeS3Opts.Credentials, storeS3Opts.ProgressChan)
	if err != nil {
		return err
	}

	log.Info().Msg("done uploading archive")
	return nil
}

// CreateFromOCIImageOptions configures OCI image indexing
type CreateFromOCIImageOptions struct {
	ImageRef      string
	OutputPath    string
	CheckpointMiB int64
	AuthConfig    string
	ProgressChan  chan<- OCIIndexProgress // optional channel for progress updates
}

// CreateFromOCIImage creates a metadata-only index (.clip) file from an OCI image
func CreateFromOCIImage(ctx context.Context, options CreateFromOCIImageOptions) error {
	log.Info().Msgf("creating OCI archive index from %s to %s", options.ImageRef, options.OutputPath)

	if options.CheckpointMiB == 0 {
		options.CheckpointMiB = 2 // default
	}

	archiver := NewClipArchiver()
	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:      options.ImageRef,
		CheckpointMiB: options.CheckpointMiB,
		AuthConfig:    options.AuthConfig,
		ProgressChan:  options.ProgressChan,
	}, options.OutputPath)

	if err != nil {
		return err
	}

	log.Info().Msg("OCI archive index created successfully")
	return nil
}

// CreateAndUploadOCIArchive creates an OCI index and uploads metadata to S3
// This combines indexing with remote storage upload
func CreateAndUploadOCIArchive(ctx context.Context, options CreateFromOCIImageOptions, si common.ClipStorageInfo) error {
	log.Info().Msgf("creating and uploading OCI archive index from %s", options.ImageRef)

	// Create the OCI index locally
	err := CreateFromOCIImage(ctx, options)
	if err != nil {
		return fmt.Errorf("failed to create OCI index: %w", err)
	}

	// If S3 storage info is provided, upload the metadata
	if _, ok := si.(*common.S3StorageInfo); ok {
		// Load the metadata
		localArchiver := NewClipArchiver()
		metadata, err := localArchiver.ExtractMetadata(options.OutputPath)
		if err != nil {
			return fmt.Errorf("failed to extract metadata: %w", err)
		}

		// Create remote archive (uploads metadata to S3)
		outputPath := options.OutputPath
		if outputPath == "" {
			outputPath = fmt.Sprintf("%s.clip", options.ImageRef)
		}

		err = localArchiver.CreateRemoteArchive(si, metadata, outputPath)
		if err != nil {
			return fmt.Errorf("failed to create remote archive: %w", err)
		}

		log.Info().Msg("OCI archive index uploaded successfully")
	}

	return nil
}
