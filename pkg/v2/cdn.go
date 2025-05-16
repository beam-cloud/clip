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

func NewCDNClipStorage(metadata *ClipV2Archive, chunkCache *ristretto.Cache[string, []byte], opts CDNClipStorageOpts) (*CDNClipStorage, error) {
	chunkPath := fmt.Sprintf("%s/chunks", opts.imageID)
	clipPath := fmt.Sprintf("%s/index.clip", opts.imageID)

	localCache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
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
	log.Printf("ReadFile START: node.ContentHash=%s, len(dest)=%d, off=%d", node.ContentHash, len(dest), off)
	// Best case, the file is small and is already in the local cache.
	if cachedContent, ok := s.localCache.Get(node.ContentHash); ok {
		log.Printf("Read local cache hit for hash: %s", node.ContentHash)
		log.Printf("ReadFile local cache: len(cachedContent)=%d", len(cachedContent))

		if off+int64(len(dest)) <= int64(len(cachedContent)) {
			n := copy(dest, cachedContent[off:off+int64(len(dest))])
			log.Printf("ReadFile local cache: copied %d bytes to dest", n)
			return n, nil
		}
		log.Printf("ReadFile local cache: offset + len(dest) > len(cachedContent), proceeding to other methods")
	}

	var (
		chunkSize = s.metadata.Header.ChunkSize
		chunks    = s.metadata.Chunks
		fileStart = node.DataPos
		fileEnd   = fileStart + node.DataLen
	)
	log.Printf("ReadFile: fileStart=%d, fileEnd=%d, node.DataLen=%d, chunkSize=%d", fileStart, fileEnd, node.DataLen, chunkSize)

	requiredChunks, err := getRequiredChunks(fileStart, chunkSize, fileEnd, chunks)
	if err != nil {
		log.Printf("ReadFile: error getting required chunks: %v", err)
		return 0, err
	}
	log.Printf("ReadFile: len(requiredChunks)=%d", len(requiredChunks))

	chunkBaseUrl := fmt.Sprintf("%s/%s", s.cdnBaseURL, s.chunkPath)
	totalBytesRead := 0

	// When the file is not in the local cache, read through the content cache.
	// Internally, the content cache will read the entire file and return it. If
	// the file is small enough, it will be cached in the local cache.
	if s.contentCache != nil && node.DataLen > 50*1024*1024 { // Use node.DataLen to check actual file size for this condition
		log.Printf("ReadFile large file: attempting content cache. node.ContentHash=%s, len(dest)=%d, off=%d", node.ContentHash, len(dest), off)
		totalBytesRead, err = s.contentCache.GetFileFromChunksWithOffset(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, off, dest)
		if err != nil {
			log.Printf("ReadFile large file: content cache error: %v", err)
			return 0, err
		}
		log.Printf("ReadFile large file, content cache hit for hash: %s. Read %d bytes.", node.ContentHash, totalBytesRead)
		return totalBytesRead, nil
	}

	// tempDest should be the size of the entire file content we expect to read
	expectedFileLength := fileEnd - fileStart
	if expectedFileLength < 0 {
		log.Printf("ReadFile: ERROR - expectedFileLength is negative: fileStart=%d, fileEnd=%d", fileStart, fileEnd)
		return 0, fmt.Errorf("calculated negative expected file length, fileStart: %d, fileEnd: %d", fileStart, fileEnd)
	}
	log.Printf("ReadFile: creating tempDest with size %d (fileEnd=%d - fileStart=%d)", expectedFileLength, fileEnd, fileStart)
	tempDest := make([]byte, expectedFileLength)

	if s.contentCache != nil {
		// If the file is small, read the entire file and cache it locally.
		log.Printf("ReadFile small file: attempting content cache. node.ContentHash=%s, len(tempDest)=%d", node.ContentHash, len(tempDest))
		// For small files, GetFileFromChunks reads the whole file into tempDest.
		bytesReadIntoTempDest, err := s.contentCache.GetFileFromChunks(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, tempDest)
		if err != nil {
			log.Printf("ReadFile small file: content cache error: %v", err)
			return 0, err
		}
		log.Printf("ReadFile small file, content cache hit for hash: %s. Read %d bytes into tempDest.", node.ContentHash, bytesReadIntoTempDest)
	} else {
		// If the file is not cached and couldn't be read through any cache, read from CDN
		log.Printf("ReadFile: no content cache or file too large for it, reading from CDN directly. node.ContentHash=%s, len(tempDest)=%d", node.ContentHash, len(tempDest))
		_, err := ReadFileChunks(s.client, ReadFileChunkRequest{
			RequiredChunks: requiredChunks,
			ChunkBaseUrl:   chunkBaseUrl,
			ChunkSize:      chunkSize,
			StartOffset:    fileStart,
			EndOffset:      fileEnd,
			ChunkCache:     s.chunkCache,
		}, tempDest)
		if err != nil {
			log.Printf("ReadFile: CDN read error: %v", err)
			return 0, err
		}
		log.Printf("ReadFile CDN hit for hash: %s", node.ContentHash)
	}

	bytesToCopy := min(int64(len(dest)), int64(len(tempDest))-off)
	log.Printf("ReadFile: len(dest)=%d, len(tempDest)=%d, off=%d, calculated bytesToCopy=%d", len(dest), len(tempDest), off, bytesToCopy)

	if bytesToCopy <= 0 {
		log.Printf("ReadFile: bytesToCopy is %d, returning 0.", bytesToCopy)
		return 0, nil // Nothing to copy
	}

	// Cache the file in the local cache
	// Only cache if we successfully read the entire file content into tempDest
	if int64(len(tempDest)) == expectedFileLength {
		s.localCache.Set(node.ContentHash, tempDest, int64(len(tempDest)))
		log.Printf("ReadFile: cached tempDest (len %d) to localCache for hash %s", len(tempDest), node.ContentHash)
	} else {
		log.Printf("ReadFile: NOT caching tempDest (len %d, expected %d) to localCache for hash %s", len(tempDest), expectedFileLength, node.ContentHash)
	}

	n := copy(dest, tempDest[off:off+bytesToCopy])
	log.Printf("ReadFile: Copied %d bytes from tempDest[off:%d] to dest. len(dest)=%d. END", n, off+bytesToCopy, len(dest))
	return n, nil
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
