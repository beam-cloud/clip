package storage

import (
	"errors"

	"github.com/beam-cloud/clip/pkg/common"
)

type ClipStorageInterface interface {
	ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error)
	Metadata() *common.ClipArchiveMetadata
}

type ClipStorageOpts interface {
}

func NewClipStorage(metadata *common.ClipArchiveMetadata, storageType string, storageOpts ClipStorageOpts) (ClipStorageInterface, error) {
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
