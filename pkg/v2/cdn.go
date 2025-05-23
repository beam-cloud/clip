package clipv2

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"syscall"
	"time"

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
	localCacheSetGroup    = singleflight.Group{}
	httpClient            = &http.Client{}
)

func init() {
	var err error
	localChunkCache, err = ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1e7, // number of keys to track frequency of (10M).
		MaxCost:     10 * 1e9,
		BufferItems: 64, // number of keys per Get buffer.
	})
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize global chunk cache")
	}

	// Client configured for quick reads from a CDN.
	httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxConnsPerHost:        100,
			MaxIdleConns:           100,
			MaxIdleConnsPerHost:    100,
			ReadBufferSize:         2 * 1024 * 1024,
			WriteBufferSize:        2 * 1024 * 1024,
			DisableCompression:     true,
			IdleConnTimeout:        90 * time.Second,
			ResponseHeaderTimeout:  10 * time.Second,
			ExpectContinueTimeout:  1 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			MaxResponseHeaderBytes: 0,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialer := &net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
					Control: func(network, address string, c syscall.RawConn) error {
						return c.Control(func(fd uintptr) {
							syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4*1024*1024)
							syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4*1024*1024)
							syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
						})
					},
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

type CDNClipStorage struct {
	cdnBaseURL         string
	imageID            string
	chunkPath          string
	clipPath           string
	metadata           *ClipV2Archive
	contentCache       ContentCache
	client             *http.Client
	localCache         *ristretto.Cache[string, []byte]
	trackChunkAccess   bool
	chunkAccessOrder   map[string]int
	chunkAccessOrderMu sync.Mutex
}

type CDNClipStorageOpts struct {
	imageID                 string
	cdnURL                  string
	contentCache            ContentCache
	chunkPriorityCallback   func(chunks []string) error
	chunkPrioritySampleTime time.Duration
}

func NewCDNClipStorage(metadata *ClipV2Archive, opts CDNClipStorageOpts) (*CDNClipStorage, error) {
	chunkPath := fmt.Sprintf("%s/chunks", opts.imageID)
	clipPath := fmt.Sprintf("%s/index.clip", opts.imageID)

	localCache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1e7,
		MaxCost:     5 * 1e9,
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}

	cdnStorage := &CDNClipStorage{
		imageID:            opts.imageID,
		cdnBaseURL:         opts.cdnURL,
		chunkPath:          chunkPath,
		clipPath:           clipPath,
		metadata:           metadata,
		contentCache:       opts.contentCache,
		client:             &http.Client{},
		localCache:         localCache,
		trackChunkAccess:   opts.chunkPriorityCallback != nil,
		chunkAccessOrder:   map[string]int{},
		chunkAccessOrderMu: sync.Mutex{},
	}

	if opts.chunkPriorityCallback != nil {
		time.AfterFunc(opts.chunkPrioritySampleTime, func() {
			log.Info().Msg("Calling chunk priority callback")
			orderedChunks := cdnStorage.orderedChunks()
			err := opts.chunkPriorityCallback(orderedChunks)
			if err != nil {
				log.Error().Err(err).Msg("Failure while calling chunk priority callback")
			}
		})
	}

	return cdnStorage, nil
}

// ReadFile reads a file from chunks stored in a CDN. It applies the requested offset to the
// clip node's start offset and begins reading len(destination buffer) bytes from that point.
func (s *CDNClipStorage) ReadFile(node *common.ClipNode, dest []byte, off int64) (int, error) {
	// Best case, the file is small and is already in the local cache.
	if cachedContent, ok := s.localCache.Get(node.ContentHash); ok {
		if off+int64(len(dest)) <= int64(len(cachedContent)) {
			log.Info().Str("hash", node.ContentHash).Msg("Read local cache hit")
			n := copy(dest, cachedContent[off:off+int64(len(dest))])
			return n, nil
		}
		log.Info().Str("hash", node.ContentHash).Msg("Read local cache hit, but not enough bytes in dest")
	}
	log.Info().Str("hash", node.ContentHash).Msg("Read local cache miss")

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

	if s.trackChunkAccess {
		s.updateChunkOrder(requiredChunks)
	}

	chunkBaseUrl := fmt.Sprintf("%s/%s", s.cdnBaseURL, s.chunkPath)

	var tempDest []byte
	if s.contentCache != nil {
		// FIXME: flip condition after testing
		if node.DataLen > 50*1e6 { // 50 MB
			log.Info().Str("hash", node.ContentHash).Str("node", node.Path).Int64("size", node.DataLen).Msg("ReadFile large file")
			n, err := s.contentCache.GetFileFromChunksWithOffset(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, off, dest)
			if err != nil {
				return 0, err
			}
			return n, nil
		}
		res, err, _ := contentCacheReadGroup.Do(node.ContentHash, func() (interface{}, error) {
			data := make([]byte, fileEnd-fileStart)
			_, fetchErr := s.contentCache.GetFileFromChunks(node.ContentHash, requiredChunks, chunkBaseUrl, chunkSize, fileStart, fileEnd, data)
			if fetchErr != nil {
				return nil, fetchErr
			}
			log.Info().Str("hash", node.ContentHash).Msg("ReadFile small file, content cache hit")
			return data, nil
		})

		if err != nil {
			return 0, err
		}

		tempDest = res.([]byte)

	} else {
		tempDest = make([]byte, fileEnd-fileStart)
		// If the file is not cached and couldn't be read through any cache, read from CDN
		_, err = ReadFileChunks(ReadFileChunkRequest{
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

	// Cache the file in the local cache, using singleflight to avoid duplicate work
	localCacheSetGroup.Do("cache:"+node.ContentHash, func() (interface{}, error) {
		if _, found := s.localCache.Get(node.ContentHash); !found {
			ok := s.localCache.SetWithTTL(node.ContentHash, tempDest, int64(len(tempDest)), time.Hour)
			if !ok {
				log.Error().Str("hash", node.ContentHash).Msg("Failed to cache file in local cache")
			}
			log.Info().Str("hash", node.ContentHash).Msg("Cached file in local cache")
		}
		return nil, nil
	})

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

func ReadFileChunks(chunkReq ReadFileChunkRequest, dest []byte) (int, error) {
	totalBytesRead := 0
	for chunkIdx, chunk := range chunkReq.RequiredChunks {
		var (
			chunkBytes []byte
			err        error
		)
		chunkURL := fmt.Sprintf("%s/%s", chunkReq.ChunkBaseUrl, chunk)

		chunkBytes, err = GetChunk(chunkURL)
		if err != nil {
			return 0, err
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

func GetChunk(chunkURL string) ([]byte, error) {
	if content, ok := localChunkCache.Get(chunkURL); ok {
		log.Info().Str("chunk", chunkURL).Msg("ReadFileChunks: Local chunk cache hit")
		return content, nil
	}
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
		// Only cache if not already present to avoid ristretto async issues
		if _, found := localChunkCache.Get(chunkURL); !found {
			localChunkCache.SetWithTTL(chunkURL, fullChunkBytes, int64(len(fullChunkBytes)), time.Hour)
		}
		log.Info().Str("chunk", chunkURL).Int("size", len(fullChunkBytes)).Msg("ReadFileChunks: Fetched and cached chunk from CDN")
		return fullChunkBytes, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
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

func (s *CDNClipStorage) updateChunkOrder(requiredChunks []string) {
	s.chunkAccessOrderMu.Lock()
	for _, chunk := range requiredChunks {
		if _, ok := s.chunkAccessOrder[chunk]; ok {
			continue
		}
		s.chunkAccessOrder[chunk] = len(s.chunkAccessOrder)
	}
	s.chunkAccessOrderMu.Unlock()
}

func (s *CDNClipStorage) orderedChunks() []string {
	s.chunkAccessOrderMu.Lock()
	defer s.chunkAccessOrderMu.Unlock()

	orderedChunks := make([]string, 0, len(s.chunkAccessOrder))
	for chunk := range s.chunkAccessOrder {
		orderedChunks = append(orderedChunks, chunk)
	}
	sort.Slice(orderedChunks, func(i, j int) bool {
		return s.chunkAccessOrder[orderedChunks[i]] < s.chunkAccessOrder[orderedChunks[j]]
	})

	return orderedChunks
}
