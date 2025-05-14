package clipv2

import (
	"fmt"
	"io"
	"net/http"
	"sync"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
)

type CDNClipStorage struct {
	cdnBaseURL   string
	imageID      string
	chunkPath    string
	clipPath     string
	metadata     *ClipV2Archive
	contentCache ContentCache
	client       *http.Client
	localCache   sync.Map
}

type CDNClipStorageOpts struct {
	imageID      string
	cdnURL       string
	contentCache ContentCache
}

func NewCDNClipStorage(metadata *ClipV2Archive, opts CDNClipStorageOpts) *CDNClipStorage {
	chunkPath := fmt.Sprintf("%s/chunks", opts.imageID)
	clipPath := fmt.Sprintf("%s/index.clip", opts.imageID)

	return &CDNClipStorage{
		imageID:    opts.imageID,
		cdnBaseURL: opts.cdnURL,
		chunkPath:  chunkPath,
		clipPath:   clipPath,
		metadata:   metadata,
		client:     &http.Client{},
		localCache: sync.Map{},
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

	// First check if the file is in the local cache. Only small files are cached locally.
	if cachedContent, ok := s.localCache.Load(node.ContentHash); ok {
		content := cachedContent.([]byte)
		n := copy(dest, content[off:])
		return n, nil
	}

	var (
		chunkSize   = s.metadata.Header.ChunkSize
		chunks      = s.metadata.Chunks
		startOffset = node.DataPos
		endOffset   = startOffset + int64(len(dest))
	)

	startChunk, endChunk, err := getChunkIndices(startOffset, chunkSize, endOffset, chunks)
	if err != nil {
		return 0, err
	}

	requiredChunks := chunks[startChunk : endChunk+1]
	chunkBaseUrl := fmt.Sprintf("%s/%s", s.cdnBaseURL, s.chunkPath)

	// When the file is not in the local cache, read through the content cache.
	// Internally, the content cache will read the entire file and return it. If
	// the file is small enough, it will be cached in the local cache.
	totalBytesRead, err := s.contentCache.GetFileFromChunks(requiredChunks, chunkBaseUrl, chunkSize, startOffset, endOffset, dest)
	if err != nil {
		return 0, err
	}

	if totalBytesRead == 0 {
		// Worst case, the file is not local and the content cache is unavailable.
		// In this case the file is read from the CDN and the result is cached locally.
		totalBytesRead, err = ReadFileChunks(s.client, requiredChunks, chunkBaseUrl, chunkSize, startOffset, endOffset, dest)
		if err != nil {
			return 0, err
		}
	}

	if len(dest) < 10*1024*1024 {
		// Cache small files locally
		s.localCache.Store(node.ContentHash, dest)
	}

	if off != 0 {
		// Reduce the destination buffer by the offset
		dest = dest[off:]
		totalBytesRead -= int(off)
	}

	return totalBytesRead, nil
}

func ReadFileChunks(httpClient *http.Client, requiredChunks []string, chunkBaseUrl string, chunkSize int64, startOffset int64, endOffset int64, dest []byte) (int, error) {
	totalBytesRead := 0
	for chunkIdx, chunk := range requiredChunks {
		chunkURL := fmt.Sprintf("%s/%s", chunkBaseUrl, chunk)

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

		resp, err := httpClient.Do(req)
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
