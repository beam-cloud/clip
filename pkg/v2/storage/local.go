package storage

import (
	"fmt"
	"os"
	"path/filepath"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	clipv2 "github.com/beam-cloud/clip/pkg/v2"
	log "github.com/rs/zerolog/log"
)

type LocalClipStorage struct {
	archivePath string
	chunkDir    string
	metadata    *clipv2.ClipV2Archive
}

type LocalClipStorageOpts struct {
	ArchivePath string
	ChunkDir    string
}

func NewLocalClipStorage(metadata *clipv2.ClipV2Archive, opts LocalClipStorageOpts) (*LocalClipStorage, error) {
	if opts.ArchivePath == "" {
		return nil, fmt.Errorf("archive path cannot be empty")
	}
	if _, err := os.Stat(opts.ArchivePath); err != nil {
		return nil, fmt.Errorf("cannot stat metadata archive file %s: %w", opts.ArchivePath, err)
	}

	return &LocalClipStorage{
		metadata:    metadata,
		archivePath: opts.ArchivePath,
		chunkDir:    opts.ChunkDir,
	}, nil
}

func (s *LocalClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	if node.NodeType != common.FileNode {
		return 0, fmt.Errorf("cannot ReadFile on non-file node type: %s", node.NodeType)
	}
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d is invalid", off)
	}
	if len(dest) == 0 {
		return 0, nil
	}

	var (
		chunkSize            = s.metadata.Header.ChunkSize
		chunkHashes          = s.metadata.Chunks
		startOffset          = node.DataPos
		endOffset            = startOffset + node.DataLen
		startChunk           = startOffset / chunkSize
		endChunk             = endOffset / chunkSize
		bytesReadTotal int64 = 0
	)

	for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
		chunkHash := chunkHashes[chunkIdx]
		chunkPath := filepath.Join(s.chunkDir, chunkHash+clipv2.ChunkSuffix)
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			return 0, fmt.Errorf("failed to open chunk file %s: %w", chunkPath, err)
		}

		var offsetInChunk int64
		if chunkIdx == startChunk {
			offsetInChunk = startOffset % chunkSize
		} else {
			offsetInChunk = 0
		}

		chunkBytesToRead := min(chunkSize-offsetInChunk, node.DataLen-bytesReadTotal)

		bytesRead, err := chunkFile.ReadAt(dest[bytesReadTotal:bytesReadTotal+chunkBytesToRead], offsetInChunk)
		if err != nil {
			chunkFile.Close()
			return 0, fmt.Errorf("failed to read chunk file %s: %w", chunkPath, err)
		}

		bytesReadTotal += int64(bytesRead)
		chunkFile.Close()
	}

	expectedReadBytes := int(endOffset - startOffset)
	if bytesReadTotal != int64(expectedReadBytes) {
		log.Warn().Msgf("ReadFile for node %s (size %d): Read %d bytes, but expected %d bytes for range [%d, %d)",
			node.Path, node.DataLen, bytesReadTotal, expectedReadBytes, startOffset, endOffset)
	}

	log.Info().Msgf("ReadFile: returning bytesReadTotal: %d", bytesReadTotal)
	return int(bytesReadTotal), nil
}

func (s *LocalClipStorage) CachedLocally() bool {
	return true
}

func (s *LocalClipStorage) Metadata() storage.ClipStorageMetadata {
	return s.metadata
}

func (s *LocalClipStorage) Cleanup() error {
	return nil
}
