package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/okteto/okteto/pkg/log"
)

type S3ClipStorage struct {
	svc                *s3.Client
	bucket             string
	key                string
	accessKey          string
	secretKey          string
	metadata           *common.ClipArchiveMetadata
	lastDownloadedByte int64
	downloadedLock     sync.Mutex
	localCachePath     string
}

type S3ClipStorageOpts struct {
	Bucket    string
	Key       string
	Region    string
	CachePath string
}

const chunkSize int64 = int64(1024 * 1024 * 100) // 100 MB

func NewS3ClipStorage(metadata *common.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	cfg, err := getAWSConfig(accessKey, secretKey, opts.Region)
	if err != nil {
		return nil, err
	}

	svc := s3.NewFromConfig(cfg)

	// Check to see if we have access to the bucket
	_, err = svc.HeadBucket(context.TODO(), &s3.HeadBucketInput{
		Bucket: aws.String(opts.Bucket),
	})

	if err != nil {
		return nil, fmt.Errorf("cannot access bucket <%s>: %v", opts.Bucket, err)
	}

	c := &S3ClipStorage{
		svc:            svc,
		bucket:         opts.Bucket,
		key:            opts.Key,
		accessKey:      accessKey,
		secretKey:      secretKey,
		metadata:       metadata,
		localCachePath: opts.CachePath,
	}

	if opts.CachePath != "" {
		go c.startBackgroundDownload()
	}

	return c, nil
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

func (s3c *S3ClipStorage) startBackgroundDownload() {
	chunkSize := chunkSize
	nextByte := int64(0)

	// Get total size of remote clip
	totalSize, err := s3c.getFileSize()
	if err != nil {
		// TODO: handle error
	}

	for {
		s3c.downloadedLock.Lock()
		nextByte = s3c.lastDownloadedByte + 1
		s3c.downloadedLock.Unlock()

		if nextByte > totalSize {
			break
		}

		_, err := s3c.downloadChunk(nextByte, nextByte+chunkSize-1, true)
		if err != nil {
			// TODO: handle error
		}
	}

	log.Success("Archive successfully cached.")
}

func (s3c *S3ClipStorage) getFileSize() (int64, error) {
	input := &s3.HeadObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
	}

	resp, err := s3c.svc.HeadObject(context.TODO(), input)
	if err != nil {
		return 0, err
	}

	return resp.ContentLength, nil
}

func (s3c *S3ClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	start := node.DataPos + off
	end := start + int64(len(dest)) - 1 // Byte ranges in HTTP RANGE requests are inclusive, so we have to subtract one

	// Check if we have downloaded the needed byte range before
	s3c.downloadedLock.Lock()
	if end > s3c.lastDownloadedByte || s3c.localCachePath == "" {
		s3c.downloadedLock.Unlock()

		// If we haven't, or if there's no local cache, download it from S3
		data, err := s3c.downloadChunk(start, end, false)
		if err != nil {
			return 0, err
		}

		copy(dest, data)

		return len(data), nil
	}
	s3c.downloadedLock.Unlock()

	// Read from local cache
	f, err := os.Open(s3c.localCachePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return f.ReadAt(dest, start)
}

func (s3c *S3ClipStorage) downloadChunk(start int64, end int64, isSequential bool) ([]byte, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
		Range:  aws.String(rangeHeader),
	}

	// Attempt to download chunk from S3
	resp, err := s3c.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		return nil, err
	}

	var n int

	// Write to local cache if localCachePath is set
	if s3c.localCachePath != "" {
		f, err := os.OpenFile(s3c.localCachePath, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		n, err = f.WriteAt(buf.Bytes(), start)
		if err != nil {
			return nil, err
		}
	} else {
		n = buf.Len()
	}

	// If the download is sequential, update the lastDownloadedByte
	if isSequential {
		s3c.downloadedLock.Lock()
		s3c.lastDownloadedByte = end
		s3c.downloadedLock.Unlock()
	}

	return buf.Bytes()[:n], nil
}

func (s3c *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s3c.metadata
}
