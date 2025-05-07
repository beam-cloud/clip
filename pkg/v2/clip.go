package clipv2

import (
	"context"

	"github.com/beam-cloud/clip/pkg/common"
	log "github.com/rs/zerolog/log"
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
	log.Info().Msgf("creating archive from %s to %s", options.SourcePath, options.LocalPath)

	a := NewClipV2Archiver()
	err := a.Create(ClipV2ArchiverOptions{
		IndexID:      options.IndexID,
		SourcePath:   options.SourcePath,
		LocalPath:    options.LocalPath,
		StorageMode:  options.StorageMode,
		MaxChunkSize: options.MaxChunkSize,
		Verbose:      options.Verbose,
	})
	if err != nil {
		return err
	}

	log.Info().Msg("archive created successfully")
	return nil
}

func GetMetadata(options ExtractOptions) (*ClipV2Archive, error) {
	a := NewClipV2Archiver()
	metadata, err := a.ExtractArchive(context.Background(), ClipV2ArchiverOptions{
		IndexID:     options.IndexID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageMode: options.StorageMode,
		Verbose:     options.Verbose,
	})
	return metadata, err
}

func ExpandLocalArchive(ctx context.Context, options ExtractOptions) error {
	a := NewClipV2Archiver()

	// In this case the source path is the local path to the archive
	err := a.ExpandLocalArchive(ctx, ClipV2ArchiverOptions{
		IndexID:     options.IndexID,
		LocalPath:   options.LocalPath,
		OutputPath:  options.OutputPath,
		StorageMode: options.StorageMode,
		Verbose:     options.Verbose,
	})
	if err != nil {
		return err
	}

	return nil
}
