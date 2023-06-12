package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/beam-cloud/clip/pkg/common"
)

type S3ClipStorage struct {
	svc       *s3.Client
	bucket    string
	key       string
	accessKey string
	secretKey string
	metadata  *common.ClipArchiveMetadata
}

type S3ClipStorageOpts struct {
	Bucket string
	Key    string
	Region string
}

func NewS3ClipStorage(metadata *common.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	cfg, err := getAWSConfig(accessKey, secretKey, opts.Region)
	if err != nil {
		return nil, err
	}

	return &S3ClipStorage{
		svc:       s3.NewFromConfig(cfg),
		bucket:    opts.Bucket,
		key:       opts.Key,
		accessKey: accessKey,
		secretKey: secretKey,
		metadata:  metadata,
	}, nil
}

func getAWSConfig(accessKey string, secretKey string, region string) (aws.Config, error) {
	var cfg aws.Config
	var err error

	if accessKey == "" || secretKey == "" {
		cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	} else {
		credentials := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")
		cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region), config.WithCredentialsProvider(credentials))
	}

	return cfg, err
}

func (s3c *S3ClipStorage) Upload(archivePath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("failed to open archive <%s>: %v", archivePath, err)
	}
	defer f.Close()

	input := &s3.PutObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
		Body:   f,
	}

	_, err = s3c.svc.PutObject(context.TODO(), input)
	if err != nil {
		return fmt.Errorf("failed to upload archive: %v", err)
	}

	return nil
}

func (s3c *S3ClipStorage) Download(objectKey string, destPath string) error {
	// TODO: Implement full download of the original archive
	return nil
}

func (s3c *S3ClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	start := node.DataPos + off
	end := start + int64(len(dest)) - 1 // Byte ranges in HTTP RANGE requests are inclusive, so we have to subtract one
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

	if end-start+1 > int64(len(dest)) {
		return 0, errors.New("dest slice is too small")
	}

	buf := bytes.NewBuffer(dest[:0]) // clear buffer but keep capacity
	n, err := io.Copy(buf, resp.Body)
	if err != nil {
		return 0, err
	}

	return int(n), nil
}

func (s3c *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s3c.metadata
}
