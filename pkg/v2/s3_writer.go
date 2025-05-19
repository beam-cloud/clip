package clipv2

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go/aws"
	common "github.com/beam-cloud/clip/pkg/common"
)

type s3ChunkWriter struct {
	ctx      context.Context
	uploader *manager.Uploader
	bucket   string
	key      string
	public   bool

	buffer *bytes.Buffer
	done   chan error
}

func newS3ChunkWriter(ctx context.Context, s3Config common.S3StorageInfo, overrideKey string) (S3ChunkWriter, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(s3Config.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			s3Config.AccessKey,
			s3Config.SecretKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if s3Config.ForcePathStyle {
			o.UsePathStyle = true
		}
		o.BaseEndpoint = aws.String(s3Config.Endpoint)
	})

	uploader := manager.NewUploader(s3Client)

	key := s3Config.Key
	if overrideKey != "" {
		key = overrideKey
	}

	return &s3ChunkWriter{
		ctx:      ctx,
		uploader: uploader,
		bucket:   s3Config.Bucket,
		key:      key,
		buffer:   new(bytes.Buffer),
		public:   s3Config.Public,
		done:     make(chan error, 1),
	}, nil
}

func (s *s3ChunkWriter) Write(p []byte) (int, error) {
	n, err := s.buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("failed to write to internal buffer: %w", err)
	}
	return n, nil
}

func (s *s3ChunkWriter) Close() error {
	pubObjInput := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
		Body:   s.buffer,
	}

	if s.public {
		pubObjInput.ACL = types.ObjectCannedACLPublicRead
	}

	go func() {
		_, err := s.uploader.Upload(s.ctx, pubObjInput)
		s.done <- err
	}()

	return nil
}

func (s *s3ChunkWriter) WaitForCompletion() error {
	return <-s.done
}
