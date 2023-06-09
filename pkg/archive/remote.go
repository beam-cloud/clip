package archive

import (
	"errors"

	log "github.com/okteto/okteto/pkg/log"
)

type StorageInfo interface {
	Type() string
}

type S3StorageInfo struct {
	Bucket string
	Key    string
	// possibly other S3 related info (region, credentials, etc.)
}

func (ssi S3StorageInfo) Type() string {
	return "s3"
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
	_, err := rca.ClipArchiver.ExtractMetadata(ClipArchiverOptions{
		ArchivePath: opts.ArchivePath,
	})
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
