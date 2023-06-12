package storage

import (
	"fmt"
	"os"

	"github.com/beam-cloud/clip/pkg/common"
)

type LocalClipStorage struct {
	archivePath string
	metadata    *common.ClipArchiveMetadata
	fileHandle  *os.File
}

type LocalClipStorageOpts struct {
	ArchivePath string
}

func NewLocalClipStorage(metadata *common.ClipArchiveMetadata, opts LocalClipStorageOpts) (*LocalClipStorage, error) {
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

func (s *LocalClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	n, err := s.fileHandle.ReadAt(dest, node.DataPos+off)
	if err != nil {
		return n, fmt.Errorf("unable to read data from file: %w", err)
	}
	return n, nil
}

func (s *LocalClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s.metadata
}
