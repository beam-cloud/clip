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

	// This a remote archive, so we have to load that particular storage implementation
	if header.StorageInfoLength > 0 {
		storageType = common.StorageModeS3
	} else {
		storageType = common.StorageModeLocal
	}

	storageInfo := opts.Metadata.StorageInfo.(common.S3StorageInfo)
	if opts.StorageInfo != nil {
		storageInfo = *opts.StorageInfo
	}

	switch storageType {
	case common.StorageModeS3:
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
