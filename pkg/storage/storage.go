package storage

import (
	"errors"

	"github.com/beam-cloud/clip/pkg/common"
)

type ClipStorageInterface interface {
	ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error)
	Metadata() *common.ClipArchiveMetadata
	CachedLocally() bool
}

type ClipStorageCredentials struct {
	S3 *S3ClipStorageCredentials
}

func NewClipStorage(archivePath string, cachePath string, metadata *common.ClipArchiveMetadata, credentials ClipStorageCredentials) (ClipStorageInterface, error) {
	var storage ClipStorageInterface = nil
	var storageType string
	var err error = nil

	header := metadata.Header

	// This a remote archive, so we have to load that particular storage implementation
	if header.StorageInfoLength > 0 {
		storageType = metadata.StorageInfo.Type()
	} else {
		storageType = "local"
	}

	switch storageType {
	case "s3":
		storageInfo := metadata.StorageInfo.(common.S3StorageInfo)
		opts := S3ClipStorageOpts{
			Bucket:    storageInfo.Bucket,
			Region:    storageInfo.Region,
			Key:       storageInfo.Key,
			CachePath: cachePath,
			AccessKey: credentials.S3.AccessKey,
			SecretKey: credentials.S3.SecretKey,
		}
		storage, err = NewS3ClipStorage(metadata, opts)
	case "local":
		opts := LocalClipStorageOpts{
			ArchivePath: archivePath,
		}
		storage, err = NewLocalClipStorage(metadata, opts)
	default:
		err = errors.New("unsupported storage type")
	}

	if err != nil {
		return nil, err
	}

	return storage, nil
}
