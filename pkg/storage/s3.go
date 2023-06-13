package storage

import (
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
)

type S3ClipStorage struct {
	svc                *s3.Client
	bucket             string
	key                string
	accessKey          string
	secretKey          string
	metadata           *common.ClipArchiveMetadata
	localCachePath     string
	lastDownloadedByte int64
	downloadedLock     sync.Mutex
}

type S3ClipStorageOpts struct {
	Bucket         string
	Key            string
	Region         string
	LocalCachePath string
}

func NewS3ClipStorage(metadata *common.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	cfg, err := getAWSConfig(accessKey, secretKey, opts.Region)
	if err != nil {
		return nil, err
	}

	svc := s3.NewFromConfig(cfg)

	// Check bucket access
	_, err = svc.HeadBucket(context.TODO(), &s3.HeadBucketInput{
		Bucket: aws.String(opts.Bucket),
	})

	if err != nil {
		return nil, fmt.Errorf("cannot access bucket <%s>: %v", opts.Bucket, err)
	}

	storage := &S3ClipStorage{
		svc:                svc,
		bucket:             opts.Bucket,
		key:                opts.Key,
		accessKey:          accessKey,
		secretKey:          secretKey,
		metadata:           metadata,
		localCachePath:     opts.LocalCachePath,
		lastDownloadedByte: -1,
	}

	go storage.startBackgroundDownload()

	return storage, nil
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

func (s *S3ClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	start := node.DataPos + off
	end := start + int64(len(dest)) - 1 // Byte ranges in HTTP RANGE requests are inclusive, so we have to subtract one

	// Check if we have downloaded the needed byte range before
	s.downloadedLock.Lock()
	if end > s.lastDownloadedByte {
		s.downloadedLock.Unlock()

		// If we haven't, download it from S3
		data, err := s.downloadChunk(start, end)
		if err != nil {
			return 0, err
		}

		copy(dest, data)

		return len(data), nil
	}
	s.downloadedLock.Unlock()

	// Read from local cache
	f, err := os.Open(s.localCachePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return f.ReadAt(dest, start)
}

func (s *S3ClipStorage) downloadChunk(start int64, end int64) ([]byte, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
		Range:  aws.String(rangeHeader),
	}

	resp, err := s.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data := make([]byte, end-start+1)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, err
	}

	// Write to local cache
	f, err := os.OpenFile(s.localCachePath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.WriteAt(data, start); err != nil {
		return nil, err
	}

	// Update lastDownloadedByte
	s.downloadedLock.Lock()
	if end > s.lastDownloadedByte {
		s.lastDownloadedByte = end
	}
	s.downloadedLock.Unlock()

	return data, nil
}

func (s *S3ClipStorage) startBackgroundDownload() {
	chunkSize := int64(1024 * 1024) // 1 MB
	nextByte := int64(0)

	for {
		s.downloadedLock.Lock()
		nextByte = s.lastDownloadedByte + 1
		s.downloadedLock.Unlock()

		if _, err := s.downloadChunk(nextByte, nextByte+chunkSize-1); err != nil {
			// handle error
			fmt.Println(err)
			return
		}
	}
}

func (s *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s.metadata
}
