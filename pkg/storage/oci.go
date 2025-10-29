package storage

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	log "github.com/rs/zerolog/log"
)

// ContentCache interface for layer caching (e.g., blobcache)
// Supports range reads for lazy loading
type ContentCache interface {
	GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error)
	StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error)
}

// OCIClipStorage implements lazy, range-based reading from OCI registries with disk + remote caching
type OCIClipStorage struct {
	metadata            *common.ClipArchiveMetadata
	storageInfo         *common.OCIStorageInfo
	layerCache          map[string]v1.Layer
	diskCacheDir        string // Local disk cache directory for decompressed layers
	httpClient          *http.Client
	keychain            authn.Keychain
	contentCache        ContentCache // Remote content cache (blobcache)
	mu                  sync.RWMutex
	layerDecompressMu   sync.Mutex               // Prevents duplicate decompression
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

// ReadFile reads file content using ranged reads from disk or remote cache
//  1. Check disk cache (range read) - fastest, local
//  2. Check ContentCache (range read) - fast, network but only what we need
//  3. Decompress from OCI - slow, but cache entire layer for future reads
func (s *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	if node.Remote == nil {
		return 0, fmt.Errorf("legacy data storage not supported in OCI mode")
	}

	remote := node.Remote

	// Calculate read range in uncompressed layer space
	wantUStart := remote.UOffset + offset
	wantUEnd := remote.UOffset + remote.ULength

	readLen := int64(len(dest))
	if wantUStart+readLen > wantUEnd {
		readLen = wantUEnd - wantUStart
	}

	if readLen <= 0 {
		return 0, nil
	}

	metrics := common.GetGlobalMetrics()
	metrics.RecordLayerAccess(remote.LayerDigest)

	// 1. Try disk cache first (fastest - local range read)
	layerPath := s.getDiskCachePath(remote.LayerDigest)
	if _, err := os.Stat(layerPath); err == nil {
		log.Debug().Str("digest", remote.LayerDigest).Int64("offset", wantUStart).Int64("length", readLen).Msg("disk cache hit")
		return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
	}

	// 2. Try remote ContentCache range read (fast - network, but only what we need!)
	if s.contentCache != nil {
		if data, err := s.tryRangeReadFromContentCache(remote.LayerDigest, wantUStart, readLen); err == nil {
			log.Debug().Str("digest", remote.LayerDigest).Int64("offset", wantUStart).Int64("length", readLen).Msg("ContentCache range read hit")
			copy(dest, data)
			return len(data), nil
		} else {
			log.Debug().Err(err).Str("digest", remote.LayerDigest).Msg("ContentCache range read miss")
		}
	}

	// 3. Cache miss - decompress from OCI and cache entire layer (for future range reads)
	layerPath, err := s.ensureLayerCached(remote.LayerDigest)
	if err != nil {
		return 0, err
	}

	// Now read the range we need from the newly cached layer
	return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
}

// ensureLayerCached ensures the decompressed layer is available on disk, returns path
func (s *OCIClipStorage) ensureLayerCached(digest string) (string, error) {
	// Generate cache file path
	layerPath := s.getDiskCachePath(digest)

	// If cached on disk, use that first
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
	log.Debug().Str("digest", digest).Msg("decompressing layer")
	err := s.decompressAndCacheLayer(digest, layerPath)

	// Clean up in-progress tracking
	s.layerDecompressMu.Lock()
	delete(s.layersDecompressing, digest)
	close(doneChan)
	s.layerDecompressMu.Unlock()

	if err != nil {
		return "", err
	}

	return layerPath, nil
}

// getDiskCachePath returns the local disk cache path for a layer
// Uses the layer digest directly for cross-image cache sharing
func (s *OCIClipStorage) getDiskCachePath(digest string) string {
	// Layer digests are in format "sha256:abc123..."
	// Use the hex part after the colon (filesystem-safe)
	// This allows multiple CLIP images to share the same cached layer
	safeDigest := strings.ReplaceAll(digest, ":", "_")
	return filepath.Join(s.diskCacheDir, safeDigest)
}

// getContentHash extracts the hex hash from a digest (e.g., "sha256:abc123..." -> "abc123...")
// This is used for content-addressed caching in remote cache
func (s *OCIClipStorage) getContentHash(digest string) string {
	// Layer digests are in format "sha256:abc123..." or "sha1:def456..."
	// Extract just the hex part for true content-addressing
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) == 2 {
		return parts[1] // Return just the hash (abc123...)
	}
	return digest // Fallback if no colon found
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

// decompressAndCacheLayer decompresses a layer from OCI registry and caches it
// This is called when both disk cache and ContentCache miss
// The entire layer is cached so subsequent reads (on this or other nodes) can do range reads
func (s *OCIClipStorage) decompressAndCacheLayer(digest string, diskPath string) error {
	metrics := common.GetGlobalMetrics()

	// NOTE: We don't check ContentCache here for the entire layer
	// Instead, ReadFile already tried a range read from ContentCache
	// If we're here, it means the layer isn't cached anywhere - decompress from OCI

	// Fetch from OCI registry and decompress
	s.mu.RLock()
	layer, exists := s.layerCache[digest]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("layer not found: %s", digest)
	}

	inflateStart := time.Now()

	// Fetch compressed layer from OCI registry
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

	// Store in remote cache (if configured) for other workers
	if s.contentCache != nil {
		go s.storeDecompressedInRemoteCache(digest, diskPath)
	} else {
		log.Debug().Str("digest", digest).Msg("remote cache not configured, skipping remote storage")
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

// tryRangeReadFromContentCache attempts a ranged read from remote ContentCache
// This enables lazy loading: we fetch only the bytes we need, not the entire layer
func (s *OCIClipStorage) tryRangeReadFromContentCache(digest string, offset, length int64) ([]byte, error) {
	// Use just the content hash (hex part) for true content-addressing
	// This allows cross-image cache sharing (same layer digest = same cache key)
	cacheKey := s.getContentHash(digest)

	// Use GetContent for range reads (offset + length)
	data, err := s.contentCache.GetContent(cacheKey, offset, length, struct{ RoutingKey string }{})
	if err != nil {
		return nil, fmt.Errorf("ContentCache range read failed: %w", err)
	}

	log.Debug().Str("digest", digest).Int64("offset", offset).Int64("length", length).Int("bytes", len(data)).Msg("ContentCache range read success")
	return data, nil
}

// storeDecompressedInRemoteCache stores decompressed layer in remote cache (async safe)
// Stores the ENTIRE layer so other nodes can do range reads from it
func (s *OCIClipStorage) storeDecompressedInRemoteCache(digest string, diskPath string) {
	// Read entire decompressed layer from disk
	data, err := os.ReadFile(diskPath)
	if err != nil {
		log.Warn().Err(err).Str("digest", digest).Msg("failed to read disk cache for remote caching")
		return
	}

	// Use just the content hash (hex part) for true content-addressing
	// This allows cross-image cache sharing (same layer digest = same cache key)
	cacheKey := s.getContentHash(digest)

	// Store using StoreContent (streams the data)
	chunks := make(chan []byte, 1)
	chunks <- data
	close(chunks)

	_, err = s.contentCache.StoreContent(chunks, cacheKey, struct{ RoutingKey string }{})
	if err != nil {
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
