package clipv2

import (
	"fmt"
	"io"
	"log"
	"net/http"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/beam-cloud/ristretto"
)

type CDNClipStorage struct {
	cdnBaseURL   string
	imageID      string
	chunkPath    string
	clipPath     string
	metadata     *ClipV2Archive
	contentCache ContentCache
	client       *http.Client
	localCache   *ristretto.Cache[string, []byte]
	chunkCache   *ristretto.Cache[string, []byte]
}

type CDNClipStorageOpts struct {
	imageID      string
	cdnURL       string
	contentCache ContentCache
}

func NewCDNClipStorage(metadata *ClipV2Archive, localCache *ristretto.Cache[string, []byte], opts CDNClipStorageOpts) (*CDNClipStorage, error) {
	chunkPath := fmt.Sprintf("%s/chunks", opts.imageID)
	clipPath := fmt.Sprintf("%s/index.clip", opts.imageID)

	chunkCache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1e7,
		MaxCost:     1 * 1e9,
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}

	return &CDNClipStorage{
		imageID:      opts.imageID,
		cdnBaseURL:   opts.cdnURL,
		chunkPath:    chunkPath,
		clipPath:     clipPath,
		metadata:     metadata,
		contentCache: opts.contentCache,
		client:       &http.Client{},
		localCache:   localCache,
		chunkCache:   chunkCache,
	}, nil
}

// ReadFile reads a file from chunks stored in a CDN. It applies the requested offset to the
// clip node's start offset and begins reading len(destination buffer) bytes from that point.
func (s *CDNClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	var (
		chunkSize = s.metadata.Header.ChunkSize
		chunks    = s.metadata.Chunks
		fileStart = node.DataPos
		fileEnd   = fileStart + node.DataLen
	)

	requiredChunks, err := getRequiredChunks(fileStart, chunkSize, fileEnd, chunks)
	if err != nil {
		return 0, err
	}

	chunkBaseUrl := fmt.Sprintf("%s/%s", s.cdnBaseURL, s.chunkPath)
	totalBytesRead := 0

	// When the file is not in the local cache, read through the content cache.
	// Internally, the content cache will read the entire file and return it. If
	// the file is small enough, it will be cached in the local cache.
	if s.contentCache != nil && len(dest) > 50*1024*1024 {
		totalBytesRead, err = s.contentCache.GetFileFromChunksWithOffset(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, off, dest)
		if err != nil {
			return 0, err
		}
		log.Printf("ReadFile large file, content cache hit for hash: %s", node.ContentHash)
		return totalBytesRead, nil
	}

	tempDest := make([]byte, fileEnd-fileStart)

	if s.contentCache != nil {
		// If the file is small, read the entire file and cache it locally.
		tempDest := make([]byte, fileEnd-fileStart)
		_, err = s.contentCache.GetFileFromChunks(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, tempDest)
		if err != nil {
			return 0, err
		}

		s.localCache.Set(node.ContentHash, tempDest, int64(len(tempDest)))

		// Make sure we don't overflow the dest buffer
		bytesToCopy := min(int64(len(dest)), int64(len(tempDest))-off)
		if bytesToCopy <= 0 {
			return 0, nil // Nothing to copy
		}

		n := copy(dest, tempDest[off:off+bytesToCopy])
		log.Printf("ReadFile small file, content cache hit for hash: %s", node.ContentHash)
		return n, nil
	} else {
		// If the file is not cached and couldn't be read through any cache, read from CDN
		totalBytesRead, err = ReadFileChunks(s.client, ReadFileChunkRequest{
			RequiredChunks: requiredChunks,
			ChunkBaseUrl:   chunkBaseUrl,
			ChunkSize:      chunkSize,
			StartOffset:    fileStart,
			EndOffset:      fileEnd,
			ChunkCache:     s.chunkCache,
		}, tempDest)
		if err != nil {
			return 0, err
		}
		log.Printf("ReadFile CDN hit for hash: %s", node.ContentHash)
	}

	s.localCache.Set(node.ContentHash, tempDest, int64(len(tempDest)))

	// Make sure we don't overflow the dest buffer
	bytesToCopy := min(int64(len(dest)), int64(len(tempDest))-off)
	if bytesToCopy <= 0 {
		return 0, nil // Nothing to copy
	}

	return totalBytesRead, nil
}

type ReadFileChunkRequest struct {
	RequiredChunks []string
	ChunkBaseUrl   string
	ChunkSize      int64
	StartOffset    int64
	EndOffset      int64
	ChunkCache     *ristretto.Cache[string, []byte]
}

func ReadFileChunks(httpClient *http.Client, chunkReq ReadFileChunkRequest, dest []byte) (int, error) {
	totalBytesRead := 0
	for chunkIdx, chunk := range chunkReq.RequiredChunks {
		chunkURL := fmt.Sprintf("%s/%s", chunkReq.ChunkBaseUrl, chunk)

		startRangeInChunk := int64(0)
		endRangeInChunk := chunkReq.ChunkSize - 1

		if chunkIdx == 0 {
			startRangeInChunk = chunkReq.StartOffset % chunkReq.ChunkSize
		}

		if chunkIdx == len(chunkReq.RequiredChunks)-1 {
			endRangeInChunk = (chunkReq.EndOffset - 1) % chunkReq.ChunkSize
		}

		offset := int64(0)
		if chunkIdx > 0 {
			offset = (int64(chunkIdx) * chunkReq.ChunkSize) - (chunkReq.StartOffset % chunkReq.ChunkSize)
		}

		var chunkBytes []byte

		if content, ok := chunkReq.ChunkCache.Get(chunkURL); ok {
			log.Printf("ReadFileChunks: Cache hit for chunk %s", chunkURL)
			chunkBytes = content
		} else {
			log.Printf("ReadFileChunks: Cache miss for chunk %s, fetching from CDN", chunkURL)
			req, err := http.NewRequest(http.MethodGet, chunkURL, nil)
			if err != nil {
				return 0, err
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return 0, fmt.Errorf("unexpected status code %d when fetching chunk %s", resp.StatusCode, chunkURL)
			}

			fullChunkBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return 0, err
			}
			chunkReq.ChunkCache.Set(chunkURL, fullChunkBytes, int64(len(fullChunkBytes)))
			chunkBytes = fullChunkBytes
		}

		if startRangeInChunk >= int64(len(chunkBytes)) {
			continue
		}
		actualEndRangeInChunk := min(endRangeInChunk, int64(len(chunkBytes)-1))
		bytesToCopyFromChunk := actualEndRangeInChunk - startRangeInChunk + 1
		if bytesToCopyFromChunk <= 0 {
			continue
		}

		copy(dest[offset:offset+bytesToCopyFromChunk], chunkBytes[startRangeInChunk:actualEndRangeInChunk+1])
		totalBytesRead += int(bytesToCopyFromChunk)

		log.Printf("ReadFileChunks: Fetched and cached chunk %s, copied %d bytes", chunk, bytesToCopyFromChunk)
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
