package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/beam-cloud/clip/pkg/common"
)

type S3ClipStorageCredentials struct {
	AccessKey string
	SecretKey string
}

type S3ClipStorage struct {
	svc            *s3.Client
	bucket         string
	key            string
	accessKey      string
	secretKey      string
	metadata       *common.ClipArchiveMetadata
	localCachePath string
	cachedLocally  bool
}

type S3ClipStorageOpts struct {
	Bucket    string
	Key       string
	Region    string
	CachePath string
	AccessKey string
	SecretKey string
}

const backgroundDownloadStartupDelay = time.Second * 25

func NewS3ClipStorage(metadata *common.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if opts.AccessKey != "" && opts.SecretKey != "" {
		accessKey = opts.AccessKey
		secretKey = opts.SecretKey
	}

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
		cachedLocally:  false,
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

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	length := fi.Size()

	// Create an uploader with the S3 client and default options
	uploader := manager.NewUploader(s3c.svc)

	_, err = uploader.Upload(context.TODO(), &s3.PutObjectInput{
		Bucket:        aws.String(s3c.bucket),
		Key:           aws.String(s3c.key),
		Body:          f,
		ContentLength: &length,
	})
	if err != nil {
		return fmt.Errorf("failed to upload archive: %v", err)
	}

	return nil
}

func (s3c *S3ClipStorage) startBackgroundDownload() {
	totalSize, err := s3c.getFileSize()
	if err != nil {
		log.Printf("Unable to get file size: %v", err)
		return
	}

	cacheFileInfo, err := os.Stat(s3c.localCachePath)
	if err == nil {
		if cacheFileInfo.Size() == totalSize {
			log.Printf("Cache file <%s> exists.\n", s3c.localCachePath)
			s3c.cachedLocally = true
			return
		}
	}

	// Wait a bit before kicking off the background download job
	time.Sleep(backgroundDownloadStartupDelay)

	log.Printf("Caching <%s>\n", s3c.localCachePath)
	startTime := time.Now()
	downloader := manager.NewDownloader(s3c.svc)
	downloader.Concurrency = 10

	f, err := os.Create(s3c.localCachePath)
	if err != nil {
		log.Printf("Failed to create file %q, %v", s3c.localCachePath, err)
		return
	}

	_, err = downloader.Download(context.TODO(), f, &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
	})
	if err != nil {
		log.Printf("Failed to download object: %v", err)
		return
	}

	log.Printf("Archive <%v> cached in %v", s3c.localCachePath, time.Since(startTime))
	s3c.cachedLocally = true
}

func (s3c *S3ClipStorage) CachedLocally() bool {
	return s3c.cachedLocally
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

	return *resp.ContentLength, nil
}

func (s3c *S3ClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	start := node.DataPos + off
	end := start + int64(len(dest)) - 1

	if s3c.localCachePath == "" || !s3c.cachedLocally {
		data, err := s3c.downloadChunk(start, end)
		if err != nil {
			return 0, err
		}

		copy(dest, data)
		return len(data), nil
	}

	// Read from local cache
	f, err := os.Open(s3c.localCachePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return f.ReadAt(dest, start)
}

func (s3c *S3ClipStorage) downloadChunk(start int64, end int64) ([]byte, error) {
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

	return buf.Bytes()[:n], nil
}

func (s3c *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s3c.metadata
}
