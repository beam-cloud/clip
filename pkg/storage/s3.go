package storage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3ClipStorage struct {
	svc    *s3.Client
	bucket string
	key    string
}

func (s *S3ClipStorage) ReadIndex() error {
	key := "mock"
	s.key = key

	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}

	resp, err := s.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return err
	}

	return nil
}

func (s *S3ClipStorage) ReadFile(start int64, end int64) (int, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
		Range:  aws.String(rangeHeader),
	}

	resp, err := s.svc.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return 0, err
	}

	return 0, nil
}
