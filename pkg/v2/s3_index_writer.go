package clipv2

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go/aws"
)

type S3ClipStorageCredentials struct {
	AccessKey string
	SecretKey string
}

type S3IndexWriter struct {
	ctx      context.Context
	uploader *manager.Uploader
	bucket   string
	key      string
	buffer   *bytes.Buffer
}

func newS3IndexWriter(ctx context.Context, opts ClipV2ArchiverOptions) (io.WriteCloser, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(opts.S3Config.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			opts.S3Config.AccessKey,
			opts.S3Config.SecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.S3Config.ForcePathStyle {
			o.UsePathStyle = true
		}
		o.BaseEndpoint = aws.String(opts.S3Config.Endpoint)
	})

	uploader := manager.NewUploader(s3Client)

	return &S3IndexWriter{
		ctx:      ctx,
		uploader: uploader,
		bucket:   opts.S3Config.Bucket,
		key:      opts.S3Config.Key,
		buffer:   new(bytes.Buffer),
	}, nil
}

func (s *S3IndexWriter) Write(p []byte) (n int, err error) {
	n, err = s.buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("failed to write to internal buffer for index: %w", err)
	}
	return n, nil
}

func (s *S3IndexWriter) Close() error {
	_, err := s.uploader.Upload(s.ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
		Body:   s.buffer,
	})

	if err != nil {
		return fmt.Errorf("failed to upload S3 index object %s/%s using manager.Uploader: %w", s.bucket, s.key, err)
	}
	return nil
}
