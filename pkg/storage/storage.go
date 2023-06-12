package storage

import (
	"errors"

	"github.com/beam-cloud/clip/pkg/archive"
)

type ClipStorageInterface interface {
	ReadFile(string) (int, error)
	Metadata() *archive.ClipArchiveMetadata
}

type ClipStorageOpts interface {
}

func NewClipStorage(metadata *archive.ClipArchiveMetadata, storageType string, storageOpts ClipStorageOpts) (ClipStorageInterface, error) {
	var storage ClipStorageInterface = nil
	var err error = nil

	switch storageType {
	case "s3":
		storage, err = NewS3ClipStorage(metadata, storageOpts.(S3ClipStorageOpts))
	case "local":
		storage, err = NewLocalClipStorage(metadata, storageOpts.(LocalClipStorageOpts))
	default:
		err = errors.New("unsupported storage type")
	}

	if err != nil {
		return nil, err
	}

	return storage, nil
}
