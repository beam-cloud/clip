package storage

import (
	"fmt"
	"io"
	"net/http"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	clipv2 "github.com/beam-cloud/clip/pkg/v2"
)

type CDNClipStorage struct {
	cdnBaseURL string
	imageID    string
	chunkPath  string
	clipPath   string
	metadata   *clipv2.ClipV2Archive
	client     *http.Client
}

func NewCDNClipStorage(cdnURL, imageID string, metadata *clipv2.ClipV2Archive) *CDNClipStorage {
	chunkPath := fmt.Sprintf("%s/chunks", imageID)
	clipPath := fmt.Sprintf("%s/index.clip", imageID)

	return &CDNClipStorage{
		imageID:    imageID,
		cdnBaseURL: cdnURL,
		chunkPath:  chunkPath,
		clipPath:   clipPath,
		metadata:   metadata,
		client:     &http.Client{},
	}
}

// ReadFile reads a file from chunks stored in a CDN. It applies the requested offset to the
// clip node's start offset and begins reading len(destination buffer) bytes from that point.
func (s *CDNClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	err := validateReadFileInput(node, off, dest)
	if err != nil {
		return 0, err
	}

	if len(dest) == 0 {
		return 0, nil
	}

	var (
		chunkSize      = s.metadata.Header.ChunkSize
		chunks         = s.metadata.Chunks
		startOffset    = node.DataPos + off
		endOffset      = startOffset + int64(len(dest))
		totalBytesRead = 0
	)

	startChunk, endChunk, err := getChunkIndices(startOffset, chunkSize, endOffset, chunks)
	if err != nil {
		return 0, err
	}

	requiredChunks := chunks[startChunk : endChunk+1]

	for chunkIdx, chunk := range requiredChunks {
		chunkURL := fmt.Sprintf("%s/%s/%s", s.cdnBaseURL, s.chunkPath, chunk)

		// Make a range request to the CDN to get the portion of the required chunk
		// [ . . . h h ] [ h h h h h ] [ h h . . . ]
		// The range of the requested chunk will always include at least one boundary
		// (start, end, or both)

		startRange := int64(0)
		endRange := chunkSize - 1

		if chunkIdx == 0 {
			startRange = startOffset % chunkSize
		}

		if chunkIdx == len(requiredChunks)-1 {
			endRange = (endOffset - 1) % chunkSize
		}

		req, err := http.NewRequest(http.MethodGet, chunkURL, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", startRange, endRange))

		resp, err := s.client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("unexpected status code %d", resp.StatusCode)
		}

		destOffset := int64(0)
		if chunkIdx > 0 {
			// Calculate where in the destination buffer this result belongs
			// by multiplying the chunk index by the chunk size and subtracting
			// the start offset modulo the chunk size because the start offset
			// may not be aligned with the chunk boundary
			destOffset = (int64(chunkIdx) * chunkSize) - (startOffset % chunkSize)
		}

		bytesToRead := endRange - startRange + 1

		n, err := io.ReadFull(resp.Body, dest[destOffset:destOffset+bytesToRead])
		if err != nil {
			return 0, err
		}

		totalBytesRead += n
	}

	return totalBytesRead, nil
}

func (s *CDNClipStorage) CachedLocally() bool {
	return false
}

func (s *CDNClipStorage) Metadata() storage.ClipStorageMetadata {
	return s.metadata
}

func (s *CDNClipStorage) Cleanup() error {
	return nil
}
