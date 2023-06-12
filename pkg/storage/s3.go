package storage

import (
	"context"
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
	Bucket    string
	Key       string
	Region    string
	AccessKey string
	SecretKey string
}

func NewS3ClipStorage(metadata *common.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	cfg, err := getAWSConfig(opts.AccessKey, opts.SecretKey, opts.Region)
	if err != nil {
		return nil, err
	}

	return &S3ClipStorage{
		svc:       s3.NewFromConfig(cfg),
		bucket:    opts.Bucket,
		key:       opts.Key,
		accessKey: opts.AccessKey,
		secretKey: opts.SecretKey,
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
		return fmt.Errorf("failed to open archive %q, %v", archivePath, err)
	}
	defer f.Close()

	input := &s3.PutObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
		Body:   f,
	}

	_, err = s3c.svc.PutObject(context.TODO(), input)
	if err != nil {
		return fmt.Errorf("failed to upload file, %v", err)
	}

	return nil
}

func (s3c *S3ClipStorage) Download(objectKey string, destPath string) error {
	// Implement the method to download a file from S3
	return nil
}

func (s3c *S3ClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
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

func (s3c *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s3c.metadata
}
