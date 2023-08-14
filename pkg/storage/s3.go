package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"

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
	localCachePath     string
	localCacheFile     *os.File
}

type S3ClipStorageOpts struct {
	Bucket    string
	Key       string
	Region    string
	CachePath string
}

const chunkSize int64 = int64(1024 * 1024 * 200) // 200 MB

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
		svc:                svc,
		bucket:             opts.Bucket,
		key:                opts.Key,
		accessKey:          accessKey,
		secretKey:          secretKey,
		metadata:           metadata,
		localCachePath:     opts.CachePath,
		lastDownloadedByte: 0,
	}

	if opts.CachePath != "" {
		os.Remove(opts.CachePath) // Clear cache path before starting the background download

		c.localCacheFile, err = os.OpenFile(c.localCachePath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open cache file: %v", err)
		}

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

	totalSize, err := s3c.getFileSize()
	if err != nil {
		log.Fatalf("Unable to get file size: %v", err)
	}

	for {
		lastDownloadedByte := atomic.LoadInt64(&s3c.lastDownloadedByte)
		nextByte = lastDownloadedByte

		// Determine the range of the chunk to download
		endByte := nextByte + chunkSize - 1
		if endByte >= totalSize {
			endByte = totalSize - 1
		}

		if nextByte > endByte {
			break
		}

		n, err := s3c.downloadChunk(nextByte, endByte)
		if err != nil {
			log.Fatalf("Failed to download chunk: %v", err)
		}

		// Update the last downloaded byte based on the actual number of bytes downloaded
		atomic.StoreInt64(&s3c.lastDownloadedByte, nextByte+int64(n)-1)
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
	end := start + int64(len(dest)) - 1

	// Check if the local cache should be used
	if s3c.localCachePath != "" {
		lastDownloadedByte := atomic.LoadInt64(&s3c.lastDownloadedByte)

		// If the requested data is in the local cache, read it
		if end <= lastDownloadedByte {
			return s3c.localCacheFile.ReadAt(dest, start)
		}
	}

	// If the local cache is not being used or the requested data is not in the cache, download it from S3
	return s3c.downloadChunkIntoBuffer(start, end, dest)
}

func (s3c *S3ClipStorage) downloadChunkIntoBuffer(start int64, end int64, dest []byte) (int, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
		Range:  aws.String(rangeHeader),
	}

	// Attempt to download chunk from S3
	resp, err := s3c.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	n, err := io.ReadFull(resp.Body, dest)
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, err
	}

	return n, nil
}

func (s3c *S3ClipStorage) downloadChunk(start int64, end int64) (int64, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
		Range:  aws.String(rangeHeader),
	}

	// Attempt to download chunk from S3
	resp, err := s3c.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	n, err := io.Copy(buf, resp.Body)
	if err != nil {
		return 0, err
	}

	// Write to local cache
	_, err = s3c.localCacheFile.WriteAt(buf.Bytes(), start)
	if err != nil {
		return 0, err
	}

	return n, nil
}

func (s3c *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s3c.metadata
}
