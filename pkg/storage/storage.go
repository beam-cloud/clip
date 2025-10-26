package storage

import (
	"errors"

	"github.com/beam-cloud/clip/pkg/common"
)

type ClipStorageInterface interface {
	ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error)
	Metadata() *common.ClipArchiveMetadata
	CachedLocally() bool
	Cleanup() error
}

type ClipStorageCredentials struct {
	S3 *S3ClipStorageCredentials
}

type ClipStorageOpts struct {
	ArchivePath string
	CachePath   string
	Metadata    *common.ClipArchiveMetadata
	StorageInfo *common.S3StorageInfo
	Credentials ClipStorageCredentials
}

func NewClipStorage(opts ClipStorageOpts) (ClipStorageInterface, error) {
	var storage ClipStorageInterface = nil
	var err error = nil

	header := opts.Metadata.Header
	metadata := opts.Metadata

	// Determine storage type based on metadata
	if header.StorageInfoLength > 0 {
		// Check the actual storage info type
		switch metadata.StorageInfo.Type() {
		case "s3":
			if metadata.StorageInfo == nil && opts.StorageInfo == nil {
				return nil, errors.New("storage info not provided")
			}

			// If StorageInfo is passed in, we can use that to override the configuration
			// stored in the metadata. This way you can use a different bucket for the
			// archive than the one used when the archive was created.
			storageInfo := metadata.StorageInfo.(common.S3StorageInfo)
			if opts.StorageInfo != nil {
				storageInfo = *opts.StorageInfo
			}

			storage, err = NewS3ClipStorage(metadata, S3ClipStorageOpts{
				Bucket:         storageInfo.Bucket,
				Region:         storageInfo.Region,
				Key:            storageInfo.Key,
				Endpoint:       storageInfo.Endpoint,
				ForcePathStyle: storageInfo.ForcePathStyle,
				CachePath:      opts.CachePath,
				AccessKey:      opts.Credentials.S3.AccessKey,
				SecretKey:      opts.Credentials.S3.SecretKey,
			})
		case "oci":
			ociStorageInfo := metadata.StorageInfo.(*common.OCIStorageInfo)
			storage, err = NewOCIClipStorage(metadata, OCIClipStorageOpts{
				RegistryURL:    ociStorageInfo.RegistryURL,
				Repository:     ociStorageInfo.Repository,
				AuthConfigPath: ociStorageInfo.AuthConfigPath,
			})
		default:
			err = errors.New("unsupported remote storage type: " + metadata.StorageInfo.Type())
		}
	} else {
		// Local storage
		storage, err = NewLocalClipStorage(metadata, LocalClipStorageOpts{
			ArchivePath: opts.ArchivePath,
		})
	}

	if err != nil {
		return nil, err
	}

	return storage, nil
}
