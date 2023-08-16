package clip

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/clip/pkg/archive"
	"github.com/beam-cloud/clip/pkg/clipfs"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type CreateOptions struct {
	InputPath  string
	OutputPath string
	Verbose    bool
}

type CreateRemoteOptions struct {
	InputPath  string
	OutputPath string
	Verbose    bool
}

type ExtractOptions struct {
	InputFile  string
	OutputPath string
	Verbose    bool
}

type MountOptions struct {
	ArchivePath  string
	MountPoint   string
	Verbose      bool
	CachePath    string
	ContentCache clipfs.ContentCache
}

type StoreS3Options struct {
	ArchivePath string
	OutputFile  string
	Bucket      string
	Key         string
	CachePath   string
}

// Create Archive
func CreateClipArchive(options CreateOptions) error {
	log.Println("Archiving...")
	log.Printf("Creating a new archive from directory: %s\n", options.InputPath)

	a := archive.NewClipArchiver()
	err := a.Create(archive.ClipArchiverOptions{
		SourcePath: options.InputPath,
		OutputFile: options.OutputPath,
		Verbose:    options.Verbose,
	})
	if err != nil {
		return err
	}

	log.Println("Archive created successfully.")
	return nil
}

func CreateAndUploadArchive(options CreateOptions, si common.ClipStorageInfo) error {
	log.Printf("Archiving...")
	log.Printf("Creating a new archive from directory: %s\n", options.InputPath)

	// Create a temporary file for storing the clip
	tempFile, err := os.CreateTemp("", "temp-clip-*.clip")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name()) // Cleanup the temporary clip (after upload it is stored remotely)

	localArchiver := archive.NewClipArchiver()
	err = localArchiver.Create(archive.ClipArchiverOptions{
		SourcePath: options.InputPath,
		OutputFile: tempFile.Name(),
		Verbose:    options.Verbose,
	})
	if err != nil {
		return err
	}

	remoteArchiver, err := archive.NewRClipArchiver(si)
	if err != nil {
		return err
	}

	err = remoteArchiver.Create(tempFile.Name(), options.OutputPath)
	if err != nil {
		return err
	}

	log.Printf("Archive created successfully.")
	return nil
}

// Extract Archive
func ExtractClipArchive(options ExtractOptions) error {
	log.Println("Extracting...")
	log.Printf("Extracting archive: %s\n", options.InputFile)

	a := archive.NewClipArchiver()
	err := a.Extract(archive.ClipArchiverOptions{
		ArchivePath: options.InputFile,
		OutputPath:  options.OutputPath,
		Verbose:     options.Verbose,
	})

	if err != nil {
		return err
	}

	log.Println("Archive extracted successfully.")
	return nil
}

// Mount a clip archive to a directory
func MountArchive(options MountOptions) (func() error, <-chan error, error) {
	log.Printf("Mounting archive %s to %s\n", options.ArchivePath, options.MountPoint)

	if _, err := os.Stat(options.MountPoint); os.IsNotExist(err) {
		err = os.MkdirAll(options.MountPoint, 0755)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create mount point directory: %v", err)
		}
		log.Println("Mount point directory created.")
	}

	ca := archive.NewClipArchiver()
	metadata, err := ca.ExtractMetadata(options.ArchivePath)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid archive: %v", err)
	}

	s, err := storage.NewClipStorage(options.ArchivePath, options.CachePath, metadata)
	if err != nil {
		return nil, nil, fmt.Errorf("could not load storage: %v", err)
	}

	clipfs, err := clipfs.NewFileSystem(s, clipfs.ClipFileSystemOpts{Verbose: options.Verbose, ContentCache: options.ContentCache})
	if err != nil {
		return nil, nil, fmt.Errorf("could not create filesystem: %v", err)
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
		return nil, nil, fmt.Errorf("could not create server: %v", err)
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
			close(serverError)
		}()

		return nil
	}

	return startServer, serverError, nil
}

// Store CLIP in remote storage
func StoreS3(storeS3Opts StoreS3Options) error {
	log.Println("Uploading...")
	region := os.Getenv("AWS_REGION")

	// If no key is provided, use the base name of the input archive as key
	if storeS3Opts.Key == "" {
		storeS3Opts.Key = filepath.Base(storeS3Opts.ArchivePath)
	}

	storageInfo := &common.S3StorageInfo{Bucket: storeS3Opts.Bucket, Key: storeS3Opts.Key, Region: region}
	a, err := archive.NewRClipArchiver(storageInfo)
	if err != nil {
		return err
	}

	err = a.Create(storeS3Opts.ArchivePath, storeS3Opts.OutputFile)
	if err != nil {
		return err
	}

	log.Println("Done uploading.")
	return nil
}
