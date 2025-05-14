package clipv2

import (
	"errors"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	log "github.com/rs/zerolog/log"
)

type ClipStorageInterface interface {
	ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error)
	Metadata() storage.ClipStorageMetadata
	CachedLocally() bool
	Cleanup() error
}

type ClipStorageOpts struct {
	ImageID      string
	ArchivePath  string
	ChunkPath    string
	CacheLocally bool
	Metadata     *ClipV2Archive
}

func NewClipStorage(opts ClipStorageOpts) (ClipStorageInterface, error) {
	var storage ClipStorageInterface = nil
	var storageType common.StorageMode
	var err error = nil

	log.Info().Msgf("Setting up new clip storage for %s", opts.ImageID)
	header := opts.Metadata.Header
	metadata := opts.Metadata

	if header.StorageInfoLength > 0 {
		storageType = common.StorageModeS3
	} else {
		storageType = common.StorageModeLocal
	}

	log.Info().Msgf("Storage type %s", storageType)
	switch storageType {
	case common.StorageModeS3:
		storage = NewCDNClipStorage(metadata, CDNClipStorageOpts{cdnURL: "https://beam-cdn.com", imageID: opts.ImageID})
		log.Info().Msgf("Created CDN clip storage")
	case common.StorageModeLocal:
		storage, err = NewLocalClipStorage(metadata, LocalClipStorageOpts{
			ArchivePath: opts.ArchivePath,
			ChunkDir:    opts.ChunkPath,
		})
		log.Info().Msgf("Created local clip storage")
	default:
		err = errors.New("unsupported storage type")
	}

	if err != nil {
		log.Error().Err(err).Msgf("Failed to create clip storage")
		return nil, err
	}

	return storage, nil
}
