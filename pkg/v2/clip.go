package clipv2

import (
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	log "github.com/rs/zerolog/log"
)

type ClipV2 struct {
	Metadata *common.ClipArchiveMetadata
}

type CreateOptions struct {
	InputPath    string
	ChunkDir     string
	IndexPath    string
	Verbose      bool
	Credentials  storage.ClipStorageCredentials
	ProgressChan chan<- int
}

// Create Archive
func CreateArchive(options CreateOptions) error {
	log.Info().Msgf("creating archive from %s to %s", options.InputPath, options.ChunkDir)

	a := NewClipV2Archiver()
	err := a.Create(ClipV2ArchiverOptions{
		SourcePath: options.InputPath,
		ChunkDir:   options.ChunkDir,
		IndexPath:  options.IndexPath,
		Verbose:    options.Verbose,
	})
	if err != nil {
		return err
	}

	log.Info().Msg("archive created successfully")
	return nil
}
