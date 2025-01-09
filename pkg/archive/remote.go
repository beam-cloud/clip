package archive

import (
	"context"
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

func (rca *RClipArchiver) Create(ctx context.Context, archivePath string, outputPath string, credentials storage.ClipStorageCredentials, progressChan chan<- int) error {
	metadata, err := rca.ClipArchiver.ExtractMetadata(archivePath)
	if err != nil {
		return err
	}

	switch rca.StorageInfo.Type() {
	case "s3":
		var storageInfo *common.S3StorageInfo = rca.StorageInfo.(*common.S3StorageInfo)
		clipStorage, err := storage.NewS3ClipStorage(metadata, storage.S3ClipStorageOpts{
			Region:    storageInfo.Region,
			Bucket:    storageInfo.Bucket,
			Key:       storageInfo.Key,
			Endpoint:  storageInfo.Endpoint,
			AccessKey: credentials.S3.AccessKey,
			SecretKey: credentials.S3.SecretKey,
		})
		if err != nil {
			return err
		}

		log.Println("Creating an RCLIP and storing original archive on S3")
		err = rca.ClipArchiver.CreateRemoteArchive(rca.StorageInfo, metadata, outputPath)
		if err != nil {
			return err
		}
		log.Println("Archive created, uploading...")

		err = clipStorage.Upload(ctx, archivePath, progressChan)
		if err != nil {
			log.Printf("Unable to upload archive: %+v\n", err)
			os.Remove(outputPath)
			return err
		}
	default:
		return errors.New("unsupported storage type")
	}

	return nil
}
