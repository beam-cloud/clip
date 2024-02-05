package archive

import (
	"encoding/gob"
	"errors"
	"log"
	"os"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
)

func init() {
	gob.Register(&common.S3StorageInfo{})
}

type RClipArchiver struct {
	ClipArchiver *ClipArchiver
	StorageInfo  common.ClipStorageInfo
}

func NewRClipArchiver(si common.ClipStorageInfo) (*RClipArchiver, error) {
	return &RClipArchiver{
		ClipArchiver: NewClipArchiver(),
		StorageInfo:  si,
	}, nil
}

func (rca *RClipArchiver) Create(archivePath string, outputPath string) error {
	metadata, err := rca.ClipArchiver.ExtractMetadata(archivePath)
	if err != nil {
		return err
	}

	log.Printf("metadata: %+v\n", metadata)

	switch rca.StorageInfo.Type() {
	case "s3":
		var storageInfo *common.S3StorageInfo = rca.StorageInfo.(*common.S3StorageInfo)
		clipStorage, err := storage.NewS3ClipStorage(metadata, storage.S3ClipStorageOpts{
			Region: storageInfo.Region,
			Bucket: storageInfo.Bucket,
			Key:    storageInfo.Key,
		})
		if err != nil {
			return err
		}

		log.Println("Creating an RCLIP and storing original archive on S3")
		err = rca.ClipArchiver.CreateRemoteArchive(rca.StorageInfo, metadata, outputPath)
		if err != nil {
			return err
		}

		err = clipStorage.Upload(archivePath)
		if err != nil {
			os.Remove(outputPath)
			return err
		}
	default:
		return errors.New("unsupported storage type")
	}

	return nil
}
