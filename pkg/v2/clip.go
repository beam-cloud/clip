package clipv2

import (
	"context"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	log "github.com/rs/zerolog/log"
)

type ClipV2 struct {
	Metadata *common.ClipArchiveMetadata
}

type CreateOptions struct {
	IndexID      string
	InputPath    string
	LocalPath    string
	Credentials  storage.ClipStorageCredentials
	Verbose      bool
	ProgressChan chan<- int
}

type ExtractOptions struct {
	IndexID     string
	SourcePath  string
	OutputPath  string
	Credentials storage.ClipStorageCredentials
	Verbose     bool
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	log.Info().Msgf("creating archive from %s to %s", options.InputPath, options.LocalPath)

	a := NewClipV2Archiver()
	err := a.Create(ClipV2ArchiverOptions{
		IndexID:    options.IndexID,
		SourcePath: options.InputPath,
		LocalPath:  options.LocalPath,
		Verbose:    options.Verbose,
	})
	if err != nil {
		return err
	}

	log.Info().Msg("archive created successfully")
	return nil
}

func ExpandLocalArchive(ctx context.Context, options ExtractOptions) error {
	a := NewClipV2Archiver()

	// In this case the source path is the local path to the archive
	err := a.ExpandLocalArchive(ctx, ClipV2ArchiverOptions{
		IndexID:    options.IndexID,
		LocalPath:  options.SourcePath,
		OutputPath: options.OutputPath,
		Verbose:    options.Verbose,
	})
	if err != nil {
		return err
	}

	return nil
}
