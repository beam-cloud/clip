package storage

import (
	"fmt"
	"os"

	"github.com/beam-cloud/clip/pkg/archive"
)

type LocalClipStorage struct {
	archivePath string
	metadata    *archive.ClipArchiveMetadata
	fileHandle  *os.File
}

type LocalClipStorageOpts struct {
	ArchivePath string
}

func NewLocalClipStorage(metadata *archive.ClipArchiveMetadata, opts LocalClipStorageOpts) (*LocalClipStorage, error) {
	fileHandle, err := os.Open(opts.ArchivePath)
	if err != nil {
		return nil, err
	}

	return &LocalClipStorage{
		metadata:    metadata,
		archivePath: opts.ArchivePath,
		fileHandle:  fileHandle,
	}, nil
}

func (s *LocalClipStorage) ReadFile(node *archive.ClipNode, dest []byte, off int64) (int, error) {
	n, err := s.fileHandle.ReadAt(dest, node.DataPos+off)
	if err != nil {
		return n, fmt.Errorf("unable to read data from file: %w", err)
	}
	return n, nil
}

func (s *LocalClipStorage) Metadata() *archive.ClipArchiveMetadata {
	return s.metadata
}
