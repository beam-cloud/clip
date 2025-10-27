package storage

import (
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

// ContentCache interface for layer caching
type ContentCache interface {
	Get(key string) ([]byte, bool, error)
	Set(key string, data []byte) error
}

// OCIClipStorage implements lazy, range-based reading from OCI registries with content caching
type OCIClipStorage struct {
	metadata     *common.ClipArchiveMetadata
	storageInfo  *common.OCIStorageInfo
	layerCache   map[string]v1.Layer // cache of layer descriptors
	httpClient   *http.Client
	keychain     authn.Keychain
	contentCache ContentCache // remote content cache (e.g., blobcache)
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
	// Build full image reference
	imageRef := fmt.Sprintf("%s/%s:%s", s.storageInfo.RegistryURL, s.storageInfo.Repository, s.storageInfo.Reference)
	
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("failed to parse image reference: %w", err)
	}

	// Fetch image
	img, err := remote.Image(ref, remote.WithAuthFromKeychain(s.keychain), remote.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to fetch image: %w", err)
	}

	// Get layers
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("failed to get layers: %w", err)
	}

	// Cache layers by digest
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			log.Warn().Msgf("Failed to get layer digest: %v", err)
			continue
		}
		s.layerCache[digest.String()] = layer
	}

	log.Info().Msgf("Initialized %d layers for OCI image", len(s.layerCache))
	return nil
}

// ReadFile reads file content using range requests and decompression
func (s *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	// Check if this is a remote ref (v2) or legacy (v1)
	if node.Remote == nil {
		// Legacy path - not supported in OCI storage
		return 0, fmt.Errorf("legacy data storage not supported in OCI mode")
	}

	// V2 path: use RemoteRef
	remote := node.Remote
	
	// Get the layer
	s.mu.RLock()
	layer, ok := s.layerCache[remote.LayerDigest]
	s.mu.RUnlock()
	
	if !ok {
		return 0, fmt.Errorf("layer not found: %s", remote.LayerDigest)
	}

	// Gzip index exists but we don't use it in MVP (always decompress from start)
	// TODO: Use gzip index with proper zran-style checkpointing for optimization
	_, hasIndex := s.storageInfo.GzipIdxByLayer[remote.LayerDigest]
	if !hasIndex {
		return 0, fmt.Errorf("gzip index not found for layer: %s", remote.LayerDigest)
	}

	// Calculate what we want to read in uncompressed space
	wantUStart := remote.UOffset + offset
	wantUEnd := remote.UOffset + remote.ULength
	
	// Cap to what was requested
	readLen := int64(len(dest))
	if wantUStart+readLen > wantUEnd {
		readLen = wantUEnd - wantUStart
	}
	
	if readLen <= 0 {
		return 0, nil
	}

	// Record metrics
	metrics := observability.GetGlobalMetrics()
	metrics.RecordLayerAccess(remote.LayerDigest)

	// Check if entire layer is cached
	if s.contentCache != nil {
		cacheKey := fmt.Sprintf("clip:oci:layer:%s", remote.LayerDigest)
		
		cachedData, found, err := s.contentCache.Get(cacheKey)
		if err != nil {
			log.Warn().Err(err).Str("layer", remote.LayerDigest).Msg("content cache lookup failed")
		} else if found {
			log.Debug().Str("layer", remote.LayerDigest).Msg("layer cache hit")
			
			// Decompress cached data and read from it
			return s.readFromCachedLayer(cachedData, wantUStart, dest[:readLen], remote)
		}
		
		log.Debug().Str("layer", remote.LayerDigest).Msg("layer cache miss, fetching and caching")
		
		// Cache miss - fetch entire compressed layer and cache it
		return s.fetchAndCacheLayer(layer, cacheKey, wantUStart, dest[:readLen], remote, metrics)
	}

	// No content cache - fallback to direct read
	return s.readDirectly(layer, wantUStart, dest[:readLen], remote, metrics)
}

// readFromCachedLayer reads from a cached compressed layer
func (s *OCIClipStorage) readFromCachedLayer(compressedData []byte, wantUStart int64, dest []byte, remote *common.RemoteRef) (int, error) {
	// Create gzip reader from cached data
	gzr, err := gzip.NewReader(io.NopCloser(io.NewSectionReader(
		&bytesReaderAt{compressedData},
		0,
		int64(len(compressedData)),
	)))
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader from cache: %w", err)
	}
	defer gzr.Close()

	// Skip to desired offset
	if wantUStart > 0 {
		_, err = io.CopyN(io.Discard, gzr, wantUStart)
		if err != nil {
			return 0, fmt.Errorf("failed to skip to offset: %w", err)
		}
	}

	// Read the data
	nRead, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nRead, fmt.Errorf("failed to read from cached layer: %w", err)
	}

	return nRead, nil
}

// fetchAndCacheLayer fetches entire layer, caches it, and returns requested data
func (s *OCIClipStorage) fetchAndCacheLayer(layer v1.Layer, cacheKey string, wantUStart int64, dest []byte, remote *common.RemoteRef, metrics *observability.Metrics) (int, error) {
	// Fetch entire compressed layer
	compressedRC, err := layer.Compressed()
	if err != nil {
		return 0, fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	// Read entire compressed layer into memory
	compressedData, err := io.ReadAll(compressedRC)
	if err != nil {
		return 0, fmt.Errorf("failed to read compressed layer: %w", err)
	}

	// Record fetch metrics
	metrics.RecordRangeGet(remote.LayerDigest, int64(len(compressedData)))

	// Cache the compressed layer asynchronously (don't block on cache writes)
	if s.contentCache != nil {
		go func() {
			if err := s.contentCache.Set(cacheKey, compressedData); err != nil {
				log.Warn().Err(err).Str("layer", remote.LayerDigest).Msg("failed to cache layer")
			} else {
				log.Info().Str("layer", remote.LayerDigest).Int("bytes", len(compressedData)).Msg("cached compressed layer")
			}
		}()
	}

	// Now read from the fetched data
	inflateStart := time.Now()
	
	gzr, err := gzip.NewReader(io.NopCloser(io.NewSectionReader(
		&bytesReaderAt{compressedData},
		0,
		int64(len(compressedData)),
	)))
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Skip to desired offset
	if wantUStart > 0 {
		_, err = io.CopyN(io.Discard, gzr, wantUStart)
		if err != nil {
			return 0, fmt.Errorf("failed to skip to offset: %w", err)
		}
	}

	// Read the data
	nRead, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nRead, fmt.Errorf("failed to read data: %w", err)
	}

	// Record metrics
	inflateDuration := time.Since(inflateStart)
	metrics.RecordInflateCPU(inflateDuration)

	return nRead, nil
}

// readDirectly reads directly from registry without caching
func (s *OCIClipStorage) readDirectly(layer v1.Layer, wantUStart int64, dest []byte, remote *common.RemoteRef, metrics *observability.Metrics) (int, error) {
	// For MVP: Always decompress from the beginning
	// TODO: Implement proper zran-style checkpointing with window state for better performance
	compressedRC, err := s.rangeGet(layer, 0)
	if err != nil {
		return 0, fmt.Errorf("range GET failed: %w", err)
	}
	defer compressedRC.Close()

	// Decompress from start
	inflateStart := time.Now()
	gzr, err := gzip.NewReader(compressedRC)
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Discard bytes until we reach the file offset
	if wantUStart > 0 {
		discarded, err := io.CopyN(io.Discard, gzr, wantUStart)
		if err != nil {
			return 0, fmt.Errorf("failed to skip %d bytes (discarded %d): %w", wantUStart, discarded, err)
		}
	}

	// Read the actual data
	nRead, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nRead, fmt.Errorf("failed to read data: %w", err)
	}

	// Record metrics
	inflateDuration := time.Since(inflateStart)
	metrics.RecordInflateCPU(inflateDuration)
	metrics.RecordRangeGet(remote.LayerDigest, int64(nRead))

	return nRead, nil
}

// rangeGet performs an HTTP Range GET on a layer starting at compressed offset
func (s *OCIClipStorage) rangeGet(layer v1.Layer, cStart int64) (io.ReadCloser, error) {
	// Get a fresh compressed stream each time
	// Note: go-containerregistry creates a new HTTP request each time Compressed() is called,
	// so this is safe to call multiple times
	compressedRC, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("failed to get compressed layer: %w", err)
	}

	// If cStart is 0, no need to skip
	if cStart == 0 {
		return compressedRC, nil
	}

	// Discard bytes until cStart
	// TODO: For production, implement proper HTTP Range GET headers
	// to avoid fetching data we'll discard
	discarded, err := io.CopyN(io.Discard, compressedRC, cStart)
	if err != nil && err != io.EOF {
		compressedRC.Close()
		return nil, fmt.Errorf("failed to skip to offset %d (discarded %d): %w", cStart, discarded, err)
	}

	return compressedRC, nil
}

// nearestCheckpoint finds the checkpoint with the largest UOff <= wantU
func (s *OCIClipStorage) nearestCheckpoint(checkpoints []common.GzipCheckpoint, wantU int64) (cOff, uOff int64) {
	if len(checkpoints) == 0 {
		return 0, 0
	}

	// Binary search for the right checkpoint
	left, right := 0, len(checkpoints)-1
	result := 0

	for left <= right {
		mid := (left + right) / 2
		if checkpoints[mid].UOff <= wantU {
			result = mid
			left = mid + 1
		} else {
			right = mid - 1
		}
	}

	return checkpoints[result].COff, checkpoints[result].UOff
}

func (s *OCIClipStorage) Metadata() *common.ClipArchiveMetadata {
	return s.metadata
}

func (s *OCIClipStorage) CachedLocally() bool {
	return false
}

func (s *OCIClipStorage) Cleanup() error {
	// Nothing to clean up for OCI storage
	return nil
}

// bytesReaderAt implements io.ReaderAt for byte slices
type bytesReaderAt struct {
	data []byte
}

func (b *bytesReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n = copy(p, b.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	return n, err
}

// BlobFetcher interface for range requests (for future enhancements)
type BlobFetcher interface {
	RangeGet(layerDigest string, cStart int64) (io.ReadCloser, error)
}

// Ensure OCIClipStorage implements ClipStorageInterface
var _ ClipStorageInterface = (*OCIClipStorage)(nil)
