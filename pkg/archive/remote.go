package archive

import (
	"errors"

	log "github.com/okteto/okteto/pkg/log"
)

type StorageInfo interface {
	Type() string
	Encode() []byte
}

type S3StorageInfo struct {
	Bucket string
	Key    string
}

func (ssi S3StorageInfo) Type() string {
	return "s3"
}

func (ssi S3StorageInfo) Encode() []byte {
	return nil
}

type RClipArchiver struct {
	ClipArchiver *ClipArchiver
	StorageInfo  StorageInfo
}

func NewRClipArchiver(si StorageInfo) (*RClipArchiver, error) {
	return &RClipArchiver{
		ClipArchiver: NewClipArchiver(),
		StorageInfo:  si,
	}, nil
}

func (rca *RClipArchiver) Create(opts ClipArchiverOptions) error {
	_, err := rca.ClipArchiver.ExtractMetadata(opts.ArchivePath)
	if err != nil {
		return err
	}

	switch rca.StorageInfo.Type() {
	case "s3":
		log.Println("creating s3 storage RCLIP")
	default:
		return errors.New("unsupported storage type")
	}

	return nil
}

func (rca *RClipArchiver) write() error {

	return nil
}
