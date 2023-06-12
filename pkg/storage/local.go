package storage

import (
	"github.com/beam-cloud/clip/pkg/archive"
)

type LocalClipStorage struct {
	archivePath string
	metadata    *archive.ClipArchiveMetadata
}

type LocalClipStorageOpts struct {
	archivePath string
}

func NewLocalClipStorage(metadata *archive.ClipArchiveMetadata, opts LocalClipStorageOpts) (*LocalClipStorage, error) {
	return &LocalClipStorage{
		metadata:    metadata,
		archivePath: opts.archivePath,
	}, nil
}

func (s *LocalClipStorage) ReadFile(path string) (int, error) {
	return 0, nil
}

func (s *LocalClipStorage) Metadata() *archive.ClipArchiveMetadata {
	return s.metadata
}
