package storage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/beam-cloud/clip/pkg/archive"
)

type S3ClipStorage struct {
	svc      *s3.Client
	bucket   string
	key      string
	metadata *archive.ClipArchiveMetadata
}

type S3ClipStorageOpts struct {
	bucket string
	key    string
	region string
}

func NewS3ClipStorage(metadata *archive.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(opts.region))
	if err != nil {
		return nil, err
	}

	return &S3ClipStorage{
		svc:      s3.NewFromConfig(cfg),
		bucket:   opts.bucket,
		key:      opts.key,
		metadata: metadata,
	}, nil
}

func (s3c *S3ClipStorage) ReadFile(node *archive.ClipNode, dest []byte, off int64) (int, error) {
	start := 0
	end := 0
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
		Range:  aws.String(rangeHeader),
	}

	resp, err := s3c.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return 0, err
	}

	return 0, nil
}

func (s3c *S3ClipStorage) Metadata() *archive.ClipArchiveMetadata {
	return s3c.metadata
}
