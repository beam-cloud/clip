package storage

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type ClipStorageInterface interface {
	ReadIndex() error
	ReadFile(int64, int64) (int, error)
}

func NewClipStorage(bucket string, region string) (ClipStorageInterface, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		return nil, err
	}

	return &S3ClipStorage{
		svc:    s3.NewFromConfig(cfg),
		bucket: bucket,
	}, nil
}
