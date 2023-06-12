package archive

import (
	"bytes"
	"encoding/gob"
	"errors"
	"os"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	log "github.com/okteto/okteto/pkg/log"
)

type S3StorageInfo struct {
	Bucket string
	Region string
	Key    string
}

func (ssi S3StorageInfo) Type() string {
	return "s3"
}

func (ssi S3StorageInfo) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(ssi); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func init() {
	gob.Register(&S3StorageInfo{})
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

	switch rca.StorageInfo.Type() {
	case "s3":
		var storageInfo *S3StorageInfo = rca.StorageInfo.(*S3StorageInfo)

		accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
		secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

		clipStorage, err := storage.NewS3ClipStorage(metadata, storage.S3ClipStorageOpts{
			AccessKey: accessKey,
			SecretKey: secretKey,
			Region:    storageInfo.Region,
			Bucket:    storageInfo.Bucket,
			Key:       storageInfo.Key,
		})
		if err != nil {
			return err
		}

		log.Information("Creating an RCLIP and storing original archive on S3")
		err = rca.ClipArchiver.CreateRemoteArchive(rca.StorageInfo, metadata, outputPath)
		if err != nil {
			return err
		}

		err = clipStorage.Upload(outputPath)
		if err != nil {
			os.Remove(outputPath)
			return err
		}

	default:
		return errors.New("unsupported storage type")
	}

	return nil
}
