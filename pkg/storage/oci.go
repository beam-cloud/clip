package storage

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// OCIClipStorage implements lazy, range-based reading from OCI registries with disk + remote caching
type OCIClipStorage struct {
	metadata            *common.ClipArchiveMetadata
	storageInfo         *common.OCIStorageInfo
	layerCache          map[string]v1.Layer
	diskCacheDir        string          // Local disk cache directory for decompressed layers
	httpClient          *http.Client
	keychain            authn.Keychain
	contentCache        ContentCache    // Remote content cache (blobcache)
	mu                  sync.RWMutex
	layerDecompressMu   sync.Mutex // Prevents duplicate decompression
	layersDecompressing map[string]chan struct{} // Tracks in-progress decompressions
}

type OCIClipStorageOpts struct {
	Metadata     *common.ClipArchiveMetadata
	AuthConfig   string       // optional base64-encoded auth config
	ContentCache ContentCache // optional remote content cache (blobcache)
	DiskCacheDir string       // optional local disk cache directory
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

	// Setup disk cache directory
	diskCacheDir := opts.DiskCacheDir
	if diskCacheDir == "" {
		// Default to system temp dir
		diskCacheDir = filepath.Join(os.TempDir(), "clip-oci-cache")
	}
	
	// Ensure cache directory exists
	if err := os.MkdirAll(diskCacheDir, 0755); err != nil {
		log.Warn().Err(err).Str("dir", diskCacheDir).Msg("failed to create disk cache dir, will use temp")
		diskCacheDir = os.TempDir()
	}

	storage := &OCIClipStorage{
		metadata:            opts.Metadata,
		storageInfo:         &storageInfo,
		layerCache:          make(map[string]v1.Layer),
		diskCacheDir:        diskCacheDir,
		httpClient:          &http.Client{},
		keychain:            authn.DefaultKeychain,
		contentCache:        opts.ContentCache,
		layersDecompressing: make(map[string]chan struct{}),
	}
	
	log.Info().Str("cache_dir", diskCacheDir).Msg("initialized OCI storage with disk cache")

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

// ReadFile reads file content using disk + remote cache
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

	// Ensure layer is cached on disk
	layerPath, err := s.ensureLayerCached(remote.LayerDigest)
	if err != nil {
		return 0, err
	}

	// Read from disk cache file
	return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
}

// ensureLayerCached ensures the decompressed layer is available on disk, returns path
func (s *OCIClipStorage) ensureLayerCached(digest string) (string, error) {
	// Generate cache file path
	layerPath := s.getDiskCachePath(digest)
	
	// Fast path: check if already on disk
	if _, err := os.Stat(layerPath); err == nil {
		log.Debug().Str("digest", digest).Str("path", layerPath).Msg("disk cache hit")
		return layerPath, nil
	}

	// Check if another goroutine is already decompressing this layer
	s.layerDecompressMu.Lock()
	if waitChan, inProgress := s.layersDecompressing[digest]; inProgress {
		// Another goroutine is decompressing - wait for it
		s.layerDecompressMu.Unlock()
		log.Debug().Str("digest", digest).Msg("waiting for in-progress decompression")
		<-waitChan
		
		// Now it should be on disk
		if _, err := os.Stat(layerPath); err == nil {
			return layerPath, nil
		}
		return "", fmt.Errorf("decompression failed for layer: %s", digest)
	}

	// We're the first - mark as in-progress
	doneChan := make(chan struct{})
	s.layersDecompressing[digest] = doneChan
	s.layerDecompressMu.Unlock()

	// Decompress and cache the layer
	log.Info().Str("digest", digest).Msg("decompressing layer (first access)")
	err := s.decompressAndCacheLayer(digest, layerPath)
	
	// Clean up in-progress tracking
	s.layerDecompressMu.Lock()
	delete(s.layersDecompressing, digest)
	close(doneChan) // Signal waiting goroutines
	s.layerDecompressMu.Unlock()

	if err != nil {
		return "", err
	}

	return layerPath, nil
}

// getDiskCachePath returns the local disk cache path for a layer
func (s *OCIClipStorage) getDiskCachePath(digest string) string {
	// Use first 16 chars of digest for filename (safe for filesystems)
	safeDigest := digest
	if len(safeDigest) > 16 {
		// Hash the digest to get a shorter, filesystem-safe name
		h := sha256.Sum256([]byte(digest))
		safeDigest = hex.EncodeToString(h[:])[:16]
	}
	return filepath.Join(s.diskCacheDir, fmt.Sprintf("layer-%s.decompressed", safeDigest))
}

// readFromDiskCache reads data from the cached layer file
func (s *OCIClipStorage) readFromDiskCache(layerPath string, offset int64, dest []byte) (int, error) {
	f, err := os.Open(layerPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open cached layer: %w", err)
	}
	defer f.Close()

	// Seek to desired offset
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("failed to seek to offset %d: %w", offset, err)
	}

	// Read requested data
	n, err := io.ReadFull(f, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return n, fmt.Errorf("failed to read from cache: %w", err)
	}

	return n, nil
}

// decompressAndCacheLayer decompresses a layer and caches it to disk + remote
func (s *OCIClipStorage) decompressAndCacheLayer(digest string, diskPath string) error {
	metrics := observability.GetGlobalMetrics()
	
	// Try remote cache first
	if s.contentCache != nil {
		if cached, found := s.tryGetDecompressedFromRemoteCache(digest); found {
			log.Info().Str("digest", digest).Int("bytes", len(cached)).Msg("loaded from remote cache")
			// Write to disk cache
			if err := s.writeToDiskCache(diskPath, cached); err != nil {
				log.Warn().Err(err).Msg("failed to write remote cache data to disk")
			}
			return nil
		}
	}

	// Get layer descriptor
	s.mu.RLock()
	layer, exists := s.layerCache[digest]
	s.mu.RUnlock()
	
	if !exists {
		return fmt.Errorf("layer not found: %s", digest)
	}

	inflateStart := time.Now()

	// Fetch compressed layer
	compressedRC, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	// Create temp file for atomic write
	tempPath := diskPath + ".tmp"
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp cache file: %w", err)
	}
	defer os.Remove(tempPath) // Clean up on error

	// Decompress directly to disk (streaming, low memory!)
	gzr, err := gzip.NewReader(compressedRC)
	if err != nil {
		tempFile.Close()
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	written, err := io.Copy(tempFile, gzr)
	tempFile.Close()
	
	if err != nil {
		return fmt.Errorf("failed to decompress layer to disk: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, diskPath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	inflateDuration := time.Since(inflateStart)
	metrics.RecordInflateCPU(inflateDuration)
	
	log.Info().
		Str("digest", digest).
		Int64("decompressed_bytes", written).
		Str("path", diskPath).
		Dur("duration", inflateDuration).
		Msg("layer decompressed and cached to disk")

	// Async: Store in remote cache for other workers
	if s.contentCache != nil {
		go s.storeDecompressedInRemoteCache(digest, diskPath)
	}

	return nil
}

// writeToDiskCache writes data to disk cache
func (s *OCIClipStorage) writeToDiskCache(path string, data []byte) error {
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// tryGetDecompressedFromRemoteCache attempts to retrieve decompressed layer from remote cache
func (s *OCIClipStorage) tryGetDecompressedFromRemoteCache(digest string) ([]byte, bool) {
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

// storeDecompressedInRemoteCache stores decompressed layer in remote cache (async safe)
func (s *OCIClipStorage) storeDecompressedInRemoteCache(digest string, diskPath string) {
	// Read from disk
	data, err := os.ReadFile(diskPath)
	if err != nil {
		log.Warn().Err(err).Str("digest", digest).Msg("failed to read disk cache for remote caching")
		return
	}

	cacheKey := fmt.Sprintf("clip:oci:layer:decompressed:%s", digest)
	
	if err := s.contentCache.Set(cacheKey, data); err != nil {
		log.Warn().Err(err).Str("digest", digest).Msg("failed to cache to remote")
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
