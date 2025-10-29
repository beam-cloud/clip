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

// ContentCache interface for layer caching (e.g., blobcache)
type ContentCache interface {
	Get(key string) ([]byte, bool, error)
	Set(key string, data []byte) error
}

// OCIClipStorage implements lazy, range-based reading from OCI registries with content caching
type OCIClipStorage struct {
	metadata            *common.ClipArchiveMetadata
	storageInfo         *common.OCIStorageInfo
	layerCache          map[string]v1.Layer
	decompressedLayers  map[string][]byte // In-memory cache of decompressed layers
	httpClient          *http.Client
	keychain            authn.Keychain
	contentCache        ContentCache
	mu                  sync.RWMutex
	layerDecompressMu   sync.Mutex // Prevents duplicate decompression
	layersDecompressing map[string]chan struct{} // Tracks in-progress decompressions
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
		metadata:            opts.Metadata,
		storageInfo:         &storageInfo,
		layerCache:          make(map[string]v1.Layer),
		decompressedLayers:  make(map[string][]byte),
		httpClient:          &http.Client{},
		keychain:            authn.DefaultKeychain,
		contentCache:        opts.ContentCache,
		layersDecompressing: make(map[string]chan struct{}),
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

// ReadFile reads file content using decompressed layer caching
func (s *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	if node.Remote == nil {
		return 0, fmt.Errorf("legacy data storage not supported in OCI mode")
	}

	remote := node.Remote
	
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

	// Get decompressed layer (from memory cache or decompress)
	decompressed, err := s.getDecompressedLayer(remote.LayerDigest)
	if err != nil {
		return 0, err
	}

	// Simple byte slice copy from decompressed data
	if wantUStart >= int64(len(decompressed)) {
		return 0, fmt.Errorf("offset %d beyond layer size %d", wantUStart, len(decompressed))
	}

	endPos := wantUStart + readLen
	if endPos > int64(len(decompressed)) {
		endPos = int64(len(decompressed))
	}

	n := copy(dest, decompressed[wantUStart:endPos])
	return n, nil
}

// getDecompressedLayer returns the fully decompressed layer, using cache
func (s *OCIClipStorage) getDecompressedLayer(digest string) ([]byte, error) {
	// Fast path: check if already in memory
	s.mu.RLock()
	if data, exists := s.decompressedLayers[digest]; exists {
		s.mu.RUnlock()
		log.Debug().Str("digest", digest).Int("bytes", len(data)).Msg("decompressed layer cache hit")
		return data, nil
	}
	layer, layerExists := s.layerCache[digest]
	s.mu.RUnlock()

	if !layerExists {
		return nil, fmt.Errorf("layer not found: %s", digest)
	}

	// Check if another goroutine is already decompressing this layer
	s.layerDecompressMu.Lock()
	if waitChan, inProgress := s.layersDecompressing[digest]; inProgress {
		// Another goroutine is decompressing - wait for it
		s.layerDecompressMu.Unlock()
		log.Debug().Str("digest", digest).Msg("waiting for in-progress decompression")
		<-waitChan
		
		// Now it should be in cache
		s.mu.RLock()
		data, exists := s.decompressedLayers[digest]
		s.mu.RUnlock()
		if exists {
			return data, nil
		}
		return nil, fmt.Errorf("decompression failed for layer: %s", digest)
	}

	// We're the first - mark as in-progress
	doneChan := make(chan struct{})
	s.layersDecompressing[digest] = doneChan
	s.layerDecompressMu.Unlock()

	// Decompress the layer
	log.Info().Str("digest", digest).Msg("decompressing layer (first access)")
	decompressed, err := s.decompressLayer(layer, digest)
	
	// Clean up in-progress tracking
	s.layerDecompressMu.Lock()
	delete(s.layersDecompressing, digest)
	close(doneChan) // Signal waiting goroutines
	s.layerDecompressMu.Unlock()

	if err != nil {
		return nil, err
	}

	// Store in memory cache
	s.mu.Lock()
	s.decompressedLayers[digest] = decompressed
	s.mu.Unlock()

	log.Info().Str("digest", digest).Int("bytes", len(decompressed)).Msg("layer decompressed and cached")

	// Optionally store in remote cache asynchronously
	if s.contentCache != nil {
		go s.storeDecompressedInCache(digest, decompressed)
	}

	return decompressed, nil
}

// decompressLayer decompresses an entire layer
func (s *OCIClipStorage) decompressLayer(layer v1.Layer, digest string) ([]byte, error) {
	metrics := observability.GetGlobalMetrics()
	
	// Try remote cache first
	if s.contentCache != nil {
		if cached, found := s.tryGetDecompressedFromCache(digest); found {
			log.Info().Str("digest", digest).Int("bytes", len(cached)).Msg("loaded decompressed layer from remote cache")
			return cached, nil
		}
	}

	inflateStart := time.Now()

	// Fetch compressed layer
	compressedRC, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	// Decompress entire layer
	gzr, err := gzip.NewReader(compressedRC)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	decompressed, err := io.ReadAll(gzr)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress layer: %w", err)
	}

	inflateDuration := time.Since(inflateStart)
	metrics.RecordInflateCPU(inflateDuration)
	
	log.Info().
		Str("digest", digest).
		Int("decompressed_bytes", len(decompressed)).
		Dur("duration", inflateDuration).
		Msg("layer decompression complete")

	return decompressed, nil
}

// tryGetDecompressedFromCache attempts to retrieve decompressed layer from remote cache
func (s *OCIClipStorage) tryGetDecompressedFromCache(digest string) ([]byte, bool) {
	cacheKey := fmt.Sprintf("clip:oci:layer:decompressed:%s", digest)
	
	data, found, err := s.contentCache.Get(cacheKey)
	if err != nil {
		log.Debug().Err(err).Str("digest", digest).Msg("remote cache lookup error")
		return nil, false
	}
	
	if found {
		log.Debug().Str("digest", digest).Int("bytes", len(data)).Msg("remote cache hit")
		return data, true
	}
	
	return nil, false
}

// storeDecompressedInCache stores decompressed layer in remote cache (async safe)
func (s *OCIClipStorage) storeDecompressedInCache(digest string, data []byte) {
	cacheKey := fmt.Sprintf("clip:oci:layer:decompressed:%s", digest)
	
	if err := s.contentCache.Set(cacheKey, data); err != nil {
		log.Warn().Err(err).Str("digest", digest).Msg("failed to cache decompressed layer")
	} else {
		log.Info().Str("digest", digest).Int("bytes", len(data)).Msg("cached decompressed layer to remote cache")
	}
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
