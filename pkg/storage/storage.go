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
	var storageType common.StorageMode
	var err error = nil

	header := opts.Metadata.Header
	metadata := opts.Metadata

	// Determine storage type from header or metadata
	if header.StorageInfoLength > 0 {
		// Check the actual storage info type
		if metadata.StorageInfo != nil {
			switch metadata.StorageInfo.Type() {
			case "oci":
				storageType = common.StorageModeOCI
			case string(common.StorageModeS3):
				storageType = common.StorageModeS3
			default:
				storageType = common.StorageModeS3 // default to S3 for backward compatibility
			}
		} else {
			storageType = common.StorageModeS3
		}
	} else {
		storageType = common.StorageModeLocal
	}

	switch storageType {
	case common.StorageModeS3:
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
	case common.StorageModeOCI:
		storage, err = NewOCIClipStorage(OCIClipStorageOpts{
			Metadata:   metadata,
			AuthConfig: "", // TODO: pass from opts if needed
		})
	case common.StorageModeLocal:
		storage, err = NewLocalClipStorage(metadata, LocalClipStorageOpts{
			ArchivePath: opts.ArchivePath,
		})
	default:
		err = errors.New("unsupported storage type")
	}

	if err != nil {
		return nil, err
	}

	return storage, nil
}
