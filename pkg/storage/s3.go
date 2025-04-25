package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
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
	cacheFile      *os.File
}

type S3ClipStorageOpts struct {
	Bucket         string
	Key            string
	Region         string
	Endpoint       string
	CachePath      string
	AccessKey      string
	SecretKey      string
	ForcePathStyle bool
}

const backgroundDownloadStartupDelay = time.Second * 30

func NewS3ClipStorage(metadata *common.ClipArchiveMetadata, opts S3ClipStorageOpts) (*S3ClipStorage, error) {
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if opts.AccessKey != "" && opts.SecretKey != "" {
		accessKey = opts.AccessKey
		secretKey = opts.SecretKey
	}

	cfg, err := getAWSConfig(accessKey, secretKey, opts.Region, opts.Endpoint)
	if err != nil {
		return nil, err
	}

	svc := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.ForcePathStyle {
			o.UsePathStyle = true
		}
	})

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
		cacheFile:      nil,
	}

	if opts.CachePath != "" {
		cacheFile, err := os.OpenFile(opts.CachePath, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open cache file <%s>: %v", opts.CachePath, err)
		}
		c.cacheFile = cacheFile
		go c.startBackgroundDownload()
	}

	return c, nil
}

func getAWSConfig(accessKey string, secretKey string, region string, endpoint string) (aws.Config, error) {
	var cfg aws.Config
	var err error
	var endpointResolver aws.EndpointResolverWithOptions
	var useDualStack aws.DualStackEndpointState

	if endpoint != "" {
		endpointResolver = aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				URL: endpoint,
			}, nil
		})
	}

	httpClient := &http.Client{}
	if common.IsIPv6Available() {
		useDualStack = aws.DualStackEndpointStateEnabled
		ipv6Transport := &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         common.DialContextIPv6,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		httpClient.Transport = ipv6Transport
	} else {
		useDualStack = aws.DualStackEndpointStateDisabled
	}

	if accessKey == "" || secretKey == "" {
		if endpointResolver != nil {
			cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region), config.WithEndpointResolverWithOptions(endpointResolver), config.WithUseDualStackEndpoint(useDualStack), config.WithHTTPClient(httpClient))
		} else {
			cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region), config.WithUseDualStackEndpoint(useDualStack), config.WithHTTPClient(httpClient))
		}
	} else {
		credentials := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

		if endpointResolver != nil {
			cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region), config.WithCredentialsProvider(credentials), config.WithEndpointResolverWithOptions(endpointResolver), config.WithUseDualStackEndpoint(useDualStack), config.WithHTTPClient(httpClient))
		} else {
			cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region), config.WithCredentialsProvider(credentials), config.WithUseDualStackEndpoint(useDualStack), config.WithHTTPClient(httpClient))
		}
	}

	return cfg, err
}

type progressReader struct {
	file *os.File
	size int64
	read int64
	ch   chan<- int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.file.Read(p)
	if n > 0 {
		pr.read += int64(n)
		progress := int(float64(pr.read) / float64(pr.size) * 100)

		if pr.ch != nil {
			pr.ch <- progress
		}
	}
	return n, err
}

func (s3c *S3ClipStorage) Upload(ctx context.Context, archivePath string, progressChan chan<- int) error {
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

	pr := &progressReader{
		file: f,
		size: length,
		ch:   progressChan,
	}

	// Create an uploader with the S3 client
	uploader := manager.NewUploader(s3c.svc, func(u *manager.Uploader) {
		u.Concurrency = 128
	})

	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s3c.bucket),
		Key:           aws.String(s3c.key),
		Body:          pr,
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
		log.Error().Msgf("Unable to get file size: %v", err)
		return
	}

	cacheFileInfo, err := s3c.cacheFile.Stat()
	if err == nil {
		if cacheFileInfo.Size() == totalSize {
			log.Info().Msgf("Cache file <%s> exists.\n", s3c.localCachePath)
			s3c.cachedLocally = true
			return
		}
	}

	// Wait a bit before kicking off the background download job
	time.Sleep(backgroundDownloadStartupDelay)

	tmpCacheFile := fmt.Sprintf("%s.%s", s3c.localCachePath, uuid.New().String()[:6])
	lockFilePath := fmt.Sprintf("%s.lock", s3c.localCachePath)

	fileLock := flock.New(lockFilePath)

	// Attempt to acquire the lock
	locked, err := fileLock.TryLock()
	if err != nil {
		log.Error().Msgf("Error while trying to acquire file lock: %v", err)
		return
	}

	if !locked {
		log.Error().Msgf("Another process is already caching %s. Skipping download.\n", s3c.localCachePath)
		return
	}

	defer fileLock.Unlock()
	defer os.Remove(lockFilePath)

	log.Info().Msgf("Caching <%s>\n", s3c.localCachePath)
	startTime := time.Now()
	downloader := manager.NewDownloader(s3c.svc)
	downloader.Concurrency = 32

	f, err := os.Create(tmpCacheFile)
	if err != nil {
		log.Error().Msgf("Failed to create file %q, %v", s3c.localCachePath, err)
		return
	}
	defer f.Close()

	_, err = downloader.Download(context.TODO(), f, &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(s3c.key),
	})
	if err != nil {
		log.Error().Msgf("Failed to download object: %v", err)
		os.Remove(tmpCacheFile)
		return
	}

	err = os.Rename(tmpCacheFile, s3c.localCachePath)
	if err != nil {
		log.Error().Msgf("Failed to move downloaded file to cache path %q, %v", s3c.localCachePath, err)
		return
	}

	// Close open file handle after rename
	s3c.cacheFile.Close()

	// Re-open cached file
	cacheFile, err := os.OpenFile(s3c.localCachePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return
	}

	log.Info().Msgf("Archive <%v> cached in %v", s3c.localCachePath, time.Since(startTime))

	s3c.cacheFile = cacheFile
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

func (s3c *S3ClipStorage) getContentFromSource(dest []byte, start, end int64) (int, error) {
	n, err := s3c.downloadChunk(dest, start, end)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s3c *S3ClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	start := node.DataPos + off
	end := start + int64(len(dest)) - 1

	if !s3c.cachedLocally {
		return s3c.getContentFromSource(dest, start, end)
	}

	// Read from local cache
	n, err := s3c.cacheFile.ReadAt(dest, start)
	if err != nil {
		// Fall back to remote source if local cache file fails for some reason
		return s3c.getContentFromSource(dest, start, end)
	}

	return n, nil
}

func (s3c *S3ClipStorage) downloadChunk(dest []byte, start int64, end int64) (int, error) {
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
	if err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}

func (s3c *S3ClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s3c.metadata
}

func (s3c *S3ClipStorage) Cleanup() error {
	if s3c.cacheFile != nil {
		s3c.cacheFile.Close()
	}

	return nil
}
