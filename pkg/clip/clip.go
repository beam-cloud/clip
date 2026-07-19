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
	v1 "github.com/google/go-containerregistry/pkg/v1"
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
	ContentCache storage.ContentCache
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
	Context               context.Context
	ArchivePath           string
	Metadata              *common.ClipArchiveMetadata
	MountPoint            string
	CachePath             string
	ContentCache          storage.ContentCache
	ContentCacheAvailable bool
	StorageModeOverride   common.StorageMode
	StorageInfo           common.ClipStorageInfo
	Credentials           storage.ClipStorageCredentials
	UseCheckpoints        bool        // Enable checkpoint-based partial decompression for OCI layers
	RegistryCredProvider  interface{} // Registry authentication (for OCI archives)
	ReadTraceObserver     common.ReadTraceObserver
	PrepareConcurrency    int
	PrepareProgress       func(storage.PrepareProgress)
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

const immutableMetadataCacheTimeout = 24 * time.Hour

func immutableFilesystemOptions() *fs.Options {
	timeout := immutableMetadataCacheTimeout
	return &fs.Options{
		AttrTimeout:     &timeout,
		EntryTimeout:    &timeout,
		NegativeTimeout: &timeout,
	}
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	log.Info().Msgf("creating archive from %s to %s", options.InputPath, options.OutputPath)

	a := NewClipArchiver()
	err := a.Create(ClipArchiverOptions{
		SourcePath:   options.InputPath,
		OutputFile:   options.OutputPath,
		ContentCache: options.ContentCache,
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
		SourcePath:   options.InputPath,
		OutputFile:   tempFile.Name(),
		ContentCache: options.ContentCache,
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

// PrepareArchiveContent materializes archive content without mounting it. This
// lets callers keep short-lived mount coordination locks out of long download
// and decompression work.
func PrepareArchiveContent(options MountOptions) error {
	archiveStorage, err := openArchiveStorage(options)
	if err != nil {
		return err
	}
	defer archiveStorage.Cleanup()

	return prepareArchiveContent(options, archiveStorage)
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

	archiveStorage, err := openArchiveStorage(options)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := prepareArchiveContent(options, archiveStorage); err != nil {
		_ = archiveStorage.Cleanup()
		return nil, nil, nil, err
	}

	clipfs, err := NewFileSystem(archiveStorage, ClipFileSystemOpts{
		ContentCache:          options.ContentCache,
		ContentCacheAvailable: options.ContentCacheAvailable,
		ReadTraceObserver:     options.ReadTraceObserver,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not create filesystem: %v", err)
	}

	root, _ := clipfs.Root()
	server, err := fuse.NewServer(fs.NewNodeFS(root, immutableFilesystemOptions()), options.MountPoint, &fuse.MountOptions{
		MaxBackground:        512,
		DisableXAttrs:        true,
		EnableSymlinkCaching: true,
		SyncRead:             false,
		RememberInodes:       true,
		MaxWrite:             1024 * 1024,
		MaxReadAhead:         1024 * 1024,
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
			archiveStorage.Cleanup()

			close(serverError)
		}()

		return nil
	}

	return startServer, serverError, server, nil
}

func openArchiveStorage(options MountOptions) (storage.ClipStorageInterface, error) {
	metadata := options.Metadata
	if metadata == nil {
		var err error
		metadata, err = NewClipArchiver().ExtractMetadata(options.ArchivePath)
		if err != nil {
			return nil, fmt.Errorf("invalid archive: %v", err)
		}
	}

	var s3Info *common.S3StorageInfo
	if si, ok := options.StorageInfo.(*common.S3StorageInfo); ok {
		s3Info = si
	} else if si, ok := options.StorageInfo.(common.S3StorageInfo); ok {
		s3Info = &si
	}

	archiveStorage, err := storage.NewClipStorage(storage.ClipStorageOpts{
		ArchivePath:           options.ArchivePath,
		CachePath:             options.CachePath,
		Metadata:              metadata,
		StorageModeOverride:   options.StorageModeOverride,
		Credentials:           options.Credentials,
		StorageInfo:           s3Info,
		ContentCache:          options.ContentCache,
		UseCheckpoints:        options.UseCheckpoints,
		ContentCacheAvailable: options.ContentCacheAvailable,
		RegistryCredProvider:  options.RegistryCredProvider,
		ReadTraceObserver:     options.ReadTraceObserver,
	})
	if err != nil {
		return nil, fmt.Errorf("could not load storage: %v", err)
	}
	return archiveStorage, nil
}

func prepareArchiveContent(options MountOptions, archiveStorage storage.ClipStorageInterface) error {
	if options.PrepareConcurrency <= 0 {
		return nil
	}
	preparer, ok := archiveStorage.(storage.ContentPreparer)
	if !ok {
		return nil
	}

	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := preparer.Prepare(ctx, storage.PrepareOptions{
		Concurrency: options.PrepareConcurrency,
		Progress:    options.PrepareProgress,
	}); err != nil {
		return fmt.Errorf("could not prepare archive content: %v", err)
	}
	return nil
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
	ImageRef         string      // Registry image reference used for fetching and metadata
	StorageImageRef  string      // Optional: image reference to store in metadata (defaults to ImageRef)
	LocalLayoutPath  string      // Optional OCI image-layout directory to read instead of the registry
	OutputPath       string      // Path for the metadata-only .clip archive
	CheckpointMiB    int64       // Gzip checkpoint interval
	CredProvider     interface{} // Optional registry credential provider
	ProgressChan     chan<- OCIIndexProgress
	Platform         *v1.Platform
	ContentCache     storage.ContentCache    // Optional cache to warm with decompressed layer streams
	ContentCacheDir  string                  // Optional temp directory for layer cache upload spooling
	LayerIndexCache  storage.LayerIndexCache // Optional per-layer index artifact cache (skips pull+index on hit)
	IndexConcurrency int                     // Max layers indexed concurrently (default 4)
}

// CreateFromOCIImage creates a metadata-only index (.clip) file from an OCI image
func CreateFromOCIImage(ctx context.Context, options CreateFromOCIImageOptions) error {
	if options.StorageImageRef != "" && options.StorageImageRef != options.ImageRef {
		log.Debug().Msgf("creating OCI archive index: indexing from %s, storing reference to %s", options.ImageRef, options.StorageImageRef)
	} else {
		log.Debug().Msgf("creating OCI archive index from %s to %s", options.ImageRef, options.OutputPath)
	}

	if options.CheckpointMiB == 0 {
		options.CheckpointMiB = 2 // default
	}

	// Convert interface{} to RegistryCredentialProvider if provided
	var credProvider common.RegistryCredentialProvider
	if options.CredProvider != nil {
		if provider, ok := options.CredProvider.(common.RegistryCredentialProvider); ok {
			credProvider = provider
		}
	}

	archiver := NewClipArchiver()
	err := archiver.CreateFromOCI(ctx, IndexOCIImageOptions{
		ImageRef:         options.ImageRef,
		StorageImageRef:  options.StorageImageRef,
		LocalLayoutPath:  options.LocalLayoutPath,
		CheckpointMiB:    options.CheckpointMiB,
		CredProvider:     credProvider,
		ProgressChan:     options.ProgressChan,
		Platform:         options.Platform,
		ContentCache:     options.ContentCache,
		ContentCacheDir:  options.ContentCacheDir,
		LayerIndexCache:  options.LayerIndexCache,
		IndexConcurrency: options.IndexConcurrency,
	}, options.OutputPath)

	if err != nil {
		return err
	}

	log.Debug().Msg("OCI archive index created successfully")
	return nil
}

// CreateAndUploadOCIArchive creates an OCI index and uploads metadata to S3
// This combines indexing with remote storage upload
func CreateAndUploadOCIArchive(ctx context.Context, options CreateFromOCIImageOptions, si common.ClipStorageInfo) error {
	log.Debug().Msgf("creating and uploading OCI archive index from %s", options.ImageRef)

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

		log.Debug().Msg("OCI archive index uploaded successfully")
	}

	return nil
}
