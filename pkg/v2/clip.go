package clipv2

import (
	"context"

	"github.com/beam-cloud/clip/pkg/common"
)

type ClipV2 struct {
	Metadata *common.ClipArchiveMetadata
}

type CreateOptions struct {
	IndexID      string
	SourcePath   string
	LocalPath    string
	S3Config     common.S3StorageInfo
	StorageMode  StorageMode
	MaxChunkSize int64
	Verbose      bool
	ProgressChan chan<- int
}

type ExtractOptions struct {
	IndexID     string
	LocalPath   string
	OutputPath  string
	S3Config    common.S3StorageInfo
	StorageMode StorageMode
	Verbose     bool
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	a := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:      options.IndexID,
		SourcePath:   options.SourcePath,
		LocalPath:    options.LocalPath,
		StorageMode:  options.StorageMode,
		S3Config:     options.S3Config,
		MaxChunkSize: options.MaxChunkSize,
		Verbose:      options.Verbose,
	})
	return a.Create()
}

func ExtractMetadata(options ExtractOptions) (*ClipV2Archive, error) {
	a := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:     options.IndexID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageMode: options.StorageMode,
		S3Config:    options.S3Config,
		Verbose:     options.Verbose,
	})
	return a.ExtractArchive(context.Background())
}

func ExpandLocalArchive(ctx context.Context, options ExtractOptions) error {
	a := NewClipV2Archiver(ClipV2ArchiverOptions{
		IndexID:     options.IndexID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageMode: options.StorageMode,
		S3Config:    options.S3Config,
		Verbose:     options.Verbose,
	})
	return a.ExpandLocalArchive(ctx)
}
