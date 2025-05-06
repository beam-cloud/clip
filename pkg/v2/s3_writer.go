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

type S3ChunkWriter struct {
	ctx      context.Context
	uploader *manager.Uploader
	bucket   string
	key      string

	buffer *bytes.Buffer
}

func newS3ChunkWriter(ctx context.Context, opts ClipV2ArchiverOptions, overrideKey string) (io.WriteCloser, error) {
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

	key := opts.S3Config.Key
	if overrideKey != "" {
		key = overrideKey
	}

	return &S3ChunkWriter{
		ctx:      ctx,
		uploader: uploader,
		bucket:   opts.S3Config.Bucket,
		key:      key,
		buffer:   new(bytes.Buffer),
	}, nil
}

func (s *S3ChunkWriter) Write(p []byte) (int, error) {
	n, err := s.buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("failed to write to internal buffer: %w", err)
	}
	return n, nil
}

func (s *S3ChunkWriter) Close() error {
	_, err := s.uploader.Upload(s.ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
		Body:   s.buffer,
	})

	if err != nil {
		return fmt.Errorf("failed to upload S3 object %s/%s using manager.Uploader: %w", s.bucket, s.key, err)
	}

	return nil
}
