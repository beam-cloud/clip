package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/observability"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	log "github.com/rs/zerolog/log"
)

// ContentCache interface for layer caching (e.g., blobcache)
type ContentCache interface {
	Get(key string) ([]byte, bool, error)
	Set(key string, data []byte) error
}

// OCIClipStorage implements lazy, range-based reading from OCI registries with content caching
type OCIClipStorage struct {
	metadata     *common.ClipArchiveMetadata
	storageInfo  *common.OCIStorageInfo
	layerCache   map[string]v1.Layer
	httpClient   *http.Client
	keychain     authn.Keychain
	contentCache ContentCache
	mu           sync.RWMutex
}

type OCIClipStorageOpts struct {
	Metadata     *common.ClipArchiveMetadata
	AuthConfig   string       // optional base64-encoded auth config
	ContentCache ContentCache // optional remote content cache
}

func NewOCIClipStorage(opts OCIClipStorageOpts) (*OCIClipStorage, error) {
	storageInfo, ok := opts.Metadata.StorageInfo.(common.OCIStorageInfo)
	if !ok {
		storageInfoPtr, ok := opts.Metadata.StorageInfo.(*common.OCIStorageInfo)
		if !ok {
			return nil, fmt.Errorf("invalid storage info type for OCI storage")
		}
		storageInfo = *storageInfoPtr
	}

	storage := &OCIClipStorage{
		metadata:     opts.Metadata,
		storageInfo:  &storageInfo,
		layerCache:   make(map[string]v1.Layer),
		httpClient:   &http.Client{},
		keychain:     authn.DefaultKeychain,
		contentCache: opts.ContentCache,
	}

	// Pre-fetch layer descriptors
	if err := storage.initLayers(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to initialize layers: %w", err)
	}

	return storage, nil
}

// initLayers fetches layer descriptors from the registry
func (s *OCIClipStorage) initLayers(ctx context.Context) error {
	imageRef := fmt.Sprintf("%s/%s:%s", s.storageInfo.RegistryURL, s.storageInfo.Repository, s.storageInfo.Reference)
	
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("failed to parse image reference: %w", err)
	}

	img, err := remote.Image(ref, remote.WithAuthFromKeychain(s.keychain), remote.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to fetch image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("failed to get layers: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			log.Warn().Err(err).Msg("failed to get layer digest")
			continue
		}
		s.layerCache[digest.String()] = layer
	}

	log.Info().Int("layer_count", len(s.layerCache)).Msg("initialized OCI layers")
	return nil
}

// ReadFile reads file content using range requests and decompression
func (s *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	if node.Remote == nil {
		return 0, fmt.Errorf("legacy data storage not supported in OCI mode")
	}

	remote := node.Remote
	
	// Get the layer
	s.mu.RLock()
	layer, ok := s.layerCache[remote.LayerDigest]
	s.mu.RUnlock()
	
	if !ok {
		return 0, fmt.Errorf("layer not found: %s", remote.LayerDigest)
	}

	// Verify gzip index exists
	if _, hasIndex := s.storageInfo.GzipIdxByLayer[remote.LayerDigest]; !hasIndex {
		return 0, fmt.Errorf("gzip index not found for layer: %s", remote.LayerDigest)
	}

	// Calculate read range in uncompressed space
	wantUStart := remote.UOffset + offset
	wantUEnd := remote.UOffset + remote.ULength
	
	readLen := int64(len(dest))
	if wantUStart+readLen > wantUEnd {
		readLen = wantUEnd - wantUStart
	}
	
	if readLen <= 0 {
		return 0, nil
	}

	metrics := observability.GetGlobalMetrics()
	metrics.RecordLayerAccess(remote.LayerDigest)

	// Try cache first if available
	if s.contentCache != nil {
		compressedData, cacheHit := s.tryGetFromCache(remote.LayerDigest)
		if cacheHit {
			return s.decompressAndRead(compressedData, wantUStart, dest[:readLen], metrics)
		}
		
		// Cache miss - fetch, cache, and read
		return s.fetchCacheAndRead(layer, remote.LayerDigest, wantUStart, dest[:readLen], metrics)
	}

	// No cache - direct read
	return s.fetchAndRead(layer, wantUStart, dest[:readLen], metrics)
}

// tryGetFromCache attempts to retrieve compressed layer from cache
func (s *OCIClipStorage) tryGetFromCache(digest string) ([]byte, bool) {
	cacheKey := fmt.Sprintf("clip:oci:layer:%s", digest)
	
	data, found, err := s.contentCache.Get(cacheKey)
	if err != nil {
		log.Debug().Err(err).Str("digest", digest).Msg("cache lookup error")
		return nil, false
	}
	
	if found {
		log.Debug().Str("digest", digest).Int("bytes", len(data)).Msg("cache hit")
		return data, true
	}
	
	log.Debug().Str("digest", digest).Msg("cache miss")
	return nil, false
}

// fetchCacheAndRead fetches layer, stores in cache, and reads requested data
func (s *OCIClipStorage) fetchCacheAndRead(layer v1.Layer, digest string, startOffset int64, dest []byte, metrics *observability.Metrics) (int, error) {
	// Fetch entire compressed layer
	compressedData, err := s.fetchLayer(layer)
	if err != nil {
		return 0, err
	}

	metrics.RecordRangeGet(digest, int64(len(compressedData)))

	// Store in cache asynchronously (don't block on cache write failures)
	if s.contentCache != nil {
		go s.storeInCache(digest, compressedData)
	}

	// Decompress and read
	return s.decompressAndRead(compressedData, startOffset, dest, metrics)
}

// fetchAndRead fetches and reads directly without caching
func (s *OCIClipStorage) fetchAndRead(layer v1.Layer, startOffset int64, dest []byte, metrics *observability.Metrics) (int, error) {
	compressedRC, err := layer.Compressed()
	if err != nil {
		return 0, fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	inflateStart := time.Now()
	
	gzr, err := gzip.NewReader(compressedRC)
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Skip to desired offset
	if startOffset > 0 {
		if _, err := io.CopyN(io.Discard, gzr, startOffset); err != nil && err != io.EOF {
			return 0, fmt.Errorf("failed to skip to offset %d: %w", startOffset, err)
		}
	}

	// Read data
	nRead, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nRead, fmt.Errorf("failed to read data: %w", err)
	}

	metrics.RecordInflateCPU(time.Since(inflateStart))
	return nRead, nil
}

// fetchLayer fetches entire compressed layer into memory
func (s *OCIClipStorage) fetchLayer(layer v1.Layer) ([]byte, error) {
	compressedRC, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	data, err := io.ReadAll(compressedRC)
	if err != nil {
		return nil, fmt.Errorf("failed to read compressed layer: %w", err)
	}

	return data, nil
}

// storeInCache stores compressed layer in cache (async safe)
func (s *OCIClipStorage) storeInCache(digest string, data []byte) {
	cacheKey := fmt.Sprintf("clip:oci:layer:%s", digest)
	
	if err := s.contentCache.Set(cacheKey, data); err != nil {
		log.Warn().Err(err).Str("digest", digest).Msg("failed to cache layer")
	} else {
		log.Info().Str("digest", digest).Int("bytes", len(data)).Msg("cached compressed layer")
	}
}

// decompressAndRead decompresses from cached/fetched data and reads requested bytes
func (s *OCIClipStorage) decompressAndRead(compressedData []byte, startOffset int64, dest []byte, metrics *observability.Metrics) (int, error) {
	inflateStart := time.Now()
	
	gzr, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Skip to desired offset
	if startOffset > 0 {
		if _, err := io.CopyN(io.Discard, gzr, startOffset); err != nil && err != io.EOF {
			return 0, fmt.Errorf("failed to skip to offset %d: %w", startOffset, err)
		}
	}

	// Read data
	nRead, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nRead, fmt.Errorf("failed to read data: %w", err)
	}

	metrics.RecordInflateCPU(time.Since(inflateStart))
	return nRead, nil
}

func (s *OCIClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s.metadata
}

func (s *OCIClipStorage) CachedLocally() bool {
	return false
}

func (s *OCIClipStorage) Cleanup() error {
	return nil
}

// Ensure OCIClipStorage implements ClipStorageInterface
var _ ClipStorageInterface = (*OCIClipStorage)(nil)
