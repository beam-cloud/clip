package clipv2

import (
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/singleflight"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/beam-cloud/ristretto"
)

var (
	localChunkCache       *ristretto.Cache[string, []byte]
	fetchGroup            = singleflight.Group{}
	contentCacheReadGroup = singleflight.Group{}
)

func init() {
	var err error
	localChunkCache, err = ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1e7,      // number of keys to track frequency of (10M).
		MaxCost:     10 << 30, // maximum cost of cache (10GB).
		BufferItems: 64,       // number of keys per Get buffer.
	})
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize global chunk cache")
	}
}

type CDNClipStorage struct {
	cdnBaseURL   string
	imageID      string
	chunkPath    string
	clipPath     string
	metadata     *ClipV2Archive
	contentCache ContentCache
	client       *http.Client
	localCache   *ristretto.Cache[string, []byte]
}

type CDNClipStorageOpts struct {
	imageID      string
	cdnURL       string
	contentCache ContentCache
}

func NewCDNClipStorage(metadata *ClipV2Archive, opts CDNClipStorageOpts) (*CDNClipStorage, error) {
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
	}, nil
}

// ReadFile reads a file from chunks stored in a CDN. It applies the requested offset to the
// clip node's start offset and begins reading len(destination buffer) bytes from that point.
func (s *CDNClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	// Best case, the file is small and is already in the local cache.
	if cachedContent, ok := s.localCache.Get(node.ContentHash); ok {
		log.Info().Str("hash", node.ContentHash).Msg("Read local cache hit")

		if off+int64(len(dest)) <= int64(len(cachedContent)) {
			n := copy(dest, cachedContent[off:off+int64(len(dest))])
			return n, nil
		}
	}

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
	if s.contentCache != nil && len(dest) > 50*1024*1024 { // TODO: Make this threshold configurable
		totalBytesRead, err = s.contentCache.GetFileFromChunksWithOffset(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, off, dest)
		if err != nil {
			return 0, err
		}
		log.Info().Str("hash", node.ContentHash).Msg("ReadFile large file, content cache hit")
		return totalBytesRead, nil
	}

	var tempDest []byte

	if s.contentCache != nil {
		res, err, _ := contentCacheReadGroup.Do(node.ContentHash, func() (interface{}, error) {
			data := make([]byte, fileEnd-fileStart)
			_, fetchErr := s.contentCache.GetFileFromChunks(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, data)
			if fetchErr != nil {
				return nil, fetchErr
			}
			return data, nil
		})

		if err != nil {
			return 0, err
		}

		tempDest = res.([]byte)

		log.Info().Str("hash", node.ContentHash).Msg("ReadFile small file, content cache hit via singleflight")
	} else {
		tempDest = make([]byte, fileEnd-fileStart)
		// If the file is not cached and couldn't be read through any cache, read from CDN
		_, err = ReadFileChunks(s.client, ReadFileChunkRequest{
			RequiredChunks: requiredChunks,
			ChunkBaseUrl:   chunkBaseUrl,
			ChunkSize:      chunkSize,
			StartOffset:    fileStart,
			EndOffset:      fileEnd,
		}, tempDest)
		if err != nil {
			return 0, err
		}
		log.Info().Str("hash", node.ContentHash).Msg("ReadFile CDN hit")
	}

	bytesToCopy := min(int64(len(dest)), int64(len(tempDest))-off)
	if bytesToCopy <= 0 {
		return 0, nil // Nothing to copy
	}

	// Cache the file in the local cache
	s.localCache.Set(node.ContentHash, tempDest, int64(len(tempDest)))

	n := copy(dest, tempDest[off:off+bytesToCopy])
	log.Info().Str("hash", node.ContentHash).Int("bytesRead", n).Msg("ReadFile")
	return n, nil
}

type ReadFileChunkRequest struct {
	RequiredChunks []string
	ChunkBaseUrl   string
	ChunkSize      int64
	StartOffset    int64
	EndOffset      int64
}

func ReadFileChunks(httpClient *http.Client, chunkReq ReadFileChunkRequest, dest []byte) (int, error) {
	totalBytesRead := 0
	for chunkIdx, chunk := range chunkReq.RequiredChunks {
		var chunkBytes []byte
		chunkURL := fmt.Sprintf("%s/%s", chunkReq.ChunkBaseUrl, chunk)

		if content, ok := localChunkCache.Get(chunkURL); ok {
			log.Info().Str("chunk", chunkURL).Msg("ReadFileChunks: Local chunk cache hit")
			chunkBytes = content
		} else {
			v, err, _ := fetchGroup.Do(chunkURL, func() (any, error) {
				log.Info().Str("chunk", chunkURL).Msg("ReadFileChunks: Cache miss, fetching from CDN")
				req, err := http.NewRequest(http.MethodGet, chunkURL, nil)
				if err != nil {
					return nil, err
				}
				resp, err := httpClient.Do(req)
				if err != nil {
					return nil, err
				}

				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					return nil, fmt.Errorf("unexpected status code %d when fetching chunk %s", resp.StatusCode, chunkURL)
				}

				fullChunkBytes, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return nil, err
				}
				localChunkCache.Set(chunkURL, fullChunkBytes, int64(len(fullChunkBytes)))
				log.Info().Str("chunk", chunkURL).Int("size", len(fullChunkBytes)).Msg("ReadFileChunks: Fetched and cached chunk from CDN")
				return fullChunkBytes, nil
			})
			if err != nil {
				return 0, err
			}
			chunkBytes = v.([]byte)
		}

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
