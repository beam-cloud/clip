package clip

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type CreateOptions struct {
	InputPath    string
	OutputPath   string
	Verbose      bool
	Credentials  storage.ClipStorageCredentials
	ProgressChan chan<- int
	LogFunc      func(string, ...interface{})
}

type CreateRemoteOptions struct {
	InputPath  string
	OutputPath string
	Verbose    bool
	LogFunc    func(format string, v ...interface{})
}
type ExtractOptions struct {
	InputFile  string
	OutputPath string
	Verbose    bool
	LogFunc    func(format string, v ...interface{})
}

type MountOptions struct {
	ArchivePath           string
	MountPoint            string
	Verbose               bool
	CachePath             string
	ContentCache          ContentCache
	ContentCacheAvailable bool
	Credentials           storage.ClipStorageCredentials
	LogFunc               func(format string, v ...interface{})
}

type StoreS3Options struct {
	ArchivePath  string
	OutputFile   string
	Bucket       string
	Key          string
	CachePath    string
	Credentials  storage.ClipStorageCredentials
	ProgressChan chan<- int
	LogFunc      func(format string, v ...interface{})
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	if options.LogFunc != nil {
		options.LogFunc("creating a new archive from directory: %s", options.InputPath)
	}

	a := NewClipArchiver()
	err := a.Create(ClipArchiverOptions{
		SourcePath: options.InputPath,
		OutputFile: options.OutputPath,
		Verbose:    options.Verbose,
	})
	if err != nil {
		return err
	}

	if options.LogFunc != nil {
		options.LogFunc("archive created successfully")
	}

	return nil
}

func CreateAndUploadArchive(ctx context.Context, options CreateOptions, si common.ClipStorageInfo) error {
	if options.LogFunc != nil {
		options.LogFunc("creating a new archive from directory: %s", options.InputPath)
	}

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
		Verbose:    options.Verbose,
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

	if options.LogFunc != nil {
		options.LogFunc("archive created successfully")
	}

	return nil
}

// Extract Archive
func ExtractArchive(options ExtractOptions) error {
	if options.LogFunc != nil {
		options.LogFunc("extracting archive: %s", options.InputFile)
	}

	a := NewClipArchiver()
	err := a.Extract(ClipArchiverOptions{
		ArchivePath: options.InputFile,
		OutputPath:  options.OutputPath,
		Verbose:     options.Verbose,
	})

	if err != nil {
		return err
	}

	if options.LogFunc != nil {
		options.LogFunc("archive extracted successfully")
	}
	return nil
}

// Mount a clip archive to a directory
func MountArchive(options MountOptions) (func() error, <-chan error, *fuse.Server, error) {
	if options.LogFunc != nil {
		options.LogFunc("mounting archive %s to %s", options.ArchivePath, options.MountPoint)
	}

	if _, err := os.Stat(options.MountPoint); os.IsNotExist(err) {
		err = os.MkdirAll(options.MountPoint, 0755)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to create mount point directory: %v", err)
		}

		if options.LogFunc != nil {
			options.LogFunc("mount point directory created")
		}
	}

	ca := NewClipArchiver()
	metadata, err := ca.ExtractMetadata(options.ArchivePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid archive: %v", err)
	}

	s, err := storage.NewClipStorage(options.ArchivePath, options.CachePath, metadata, options.Credentials)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not load storage: %v", err)
	}

	clipfs, err := NewFileSystem(s, ClipFileSystemOpts{Verbose: options.Verbose, ContentCache: options.ContentCache, ContentCacheAvailable: options.ContentCacheAvailable})
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
		MaxReadAhead:         1 << 17,
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

			s.Cleanup()
			close(serverError)
		}()

		return nil
	}

	return startServer, serverError, server, nil
}

// Store CLIP in remote storage
func StoreS3(storeS3Opts StoreS3Options) error {
	if storeS3Opts.LogFunc != nil {
		storeS3Opts.LogFunc("uploading archive")
	}

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

	if storeS3Opts.LogFunc != nil {
		storeS3Opts.LogFunc("done uploading archive")
	}
	return nil
}
