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
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	log "github.com/rs/zerolog/log"
)

// OCIClipStorage implements lazy, range-based reading from OCI registries with disk + remote caching
type OCIClipStorage struct {
	metadata              *common.ClipArchiveMetadata
	storageInfo           *common.OCIStorageInfo
	layerCache            map[string]v1.Layer
	diskCacheDir          string // Local disk cache directory for decompressed layers
	httpClient            *http.Client
	keychain              authn.Keychain
	contentCache          ContentCache // Remote content cache (blobcache)
	mu                    sync.RWMutex
	layerDecompressMu     sync.Mutex               // Prevents duplicate decompression
	layersDecompressing   map[string]chan struct{} // Tracks in-progress decompressions
	decompressedHashCache map[string]string        // Maps layer digest -> decompressed hash
	decompressedHashMu    sync.RWMutex
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
		metadata:              opts.Metadata,
		storageInfo:           &storageInfo,
		layerCache:            make(map[string]v1.Layer),
		diskCacheDir:          diskCacheDir,
		httpClient:            &http.Client{},
		keychain:              authn.DefaultKeychain,
		contentCache:          opts.ContentCache,
		layersDecompressing:   make(map[string]chan struct{}),
		decompressedHashCache: make(map[string]string),
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

	// Get or compute the decompressed hash
	decompressedHash := s.getDecompressedHash(remote.LayerDigest)

	// 1. Try disk cache first (fastest - local range read)
	if decompressedHash != "" {
		layerPath := s.getDecompressedCachePath(decompressedHash)
		if _, err := os.Stat(layerPath); err == nil {
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Msg("DISK CACHE HIT - using local decompressed layer")
			return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
		}
	}

	// 2. Try remote ContentCache range read (fast - network, but only what we need!)
	if s.contentCache != nil && decompressedHash != "" {
		log.Debug().
			Str("layer_digest", remote.LayerDigest).
			Str("decompressed_hash", decompressedHash).
			Int64("offset", wantUStart).
			Int64("length", readLen).
			Msg("Trying ContentCache range read")
		
		if data, err := s.tryRangeReadFromContentCache(decompressedHash, wantUStart, readLen); err == nil {
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Int("bytes_read", len(data)).
				Msg("CONTENT CACHE HIT - range read from remote")
			copy(dest, data)
			return len(data), nil
		} else {
			log.Debug().
				Err(err).
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Msg("ContentCache miss - will decompress from OCI")
		}
	}

	// 3. Cache miss - decompress from OCI and cache entire layer (for future range reads)
	decompressedHash, layerPath, err := s.ensureLayerCached(remote.LayerDigest)
	if err != nil {
		return 0, err
	}

	// Now read the range we need from the newly cached layer
	return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
}

// ensureLayerCached ensures the decompressed layer is available on disk
// Returns decompressed hash and path
func (s *OCIClipStorage) ensureLayerCached(digest string) (string, string, error) {
	// Check if we already know the decompressed hash
	decompressedHash := s.getDecompressedHash(digest)
	if decompressedHash != "" {
		layerPath := s.getDecompressedCachePath(decompressedHash)
		if _, err := os.Stat(layerPath); err == nil {
			log.Debug().Str("digest", digest).Str("decompressed_hash", decompressedHash).Msg("disk cache hit")
			return decompressedHash, layerPath, nil
		}
	}

	// Check if another goroutine is already decompressing this layer
	s.layerDecompressMu.Lock()
	if waitChan, inProgress := s.layersDecompressing[digest]; inProgress {
		// Another goroutine is decompressing - wait for it
		s.layerDecompressMu.Unlock()
		log.Debug().Str("digest", digest).Msg("waiting for in-progress decompression")
		<-waitChan

		// Now get the decompressed hash and path
		decompressedHash = s.getDecompressedHash(digest)
		if decompressedHash != "" {
			layerPath := s.getDecompressedCachePath(decompressedHash)
			if _, err := os.Stat(layerPath); err == nil {
				return decompressedHash, layerPath, nil
			}
		}
		return "", "", fmt.Errorf("decompression failed for layer: %s", digest)
	}

	// We're the first - mark as in-progress
	doneChan := make(chan struct{})
	s.layersDecompressing[digest] = doneChan
	s.layerDecompressMu.Unlock()

	// Decompress and cache the layer
	log.Info().
		Str("layer_digest", digest).
		Msg("OCI CACHE MISS - downloading and decompressing layer from registry")
	
	// Use temp path, will be renamed to final path inside decompressAndCacheLayer
	tempPath := filepath.Join(s.diskCacheDir, digest+".tmp")
	decompressedHash, err := s.decompressAndCacheLayer(digest, tempPath)

	// Clean up in-progress tracking
	s.layerDecompressMu.Lock()
	delete(s.layersDecompressing, digest)
	close(doneChan)
	s.layerDecompressMu.Unlock()

	if err != nil {
		return "", "", err
	}

	layerPath := s.getDecompressedCachePath(decompressedHash)
	return decompressedHash, layerPath, nil
}

// getDecompressedCachePath returns the cache path for a decompressed hash
func (s *OCIClipStorage) getDecompressedCachePath(decompressedHash string) string {
	return filepath.Join(s.diskCacheDir, decompressedHash)
}

// getDecompressedHash retrieves the cached decompressed hash for a layer digest
func (s *OCIClipStorage) getDecompressedHash(layerDigest string) string {
	s.decompressedHashMu.RLock()
	defer s.decompressedHashMu.RUnlock()
	return s.decompressedHashCache[layerDigest]
}

// storeDecompressedHashMapping stores the mapping from layer digest to decompressed hash
func (s *OCIClipStorage) storeDecompressedHashMapping(layerDigest, decompressedHash string) {
	s.decompressedHashMu.Lock()
	defer s.decompressedHashMu.Unlock()
	s.decompressedHashCache[layerDigest] = decompressedHash
	log.Debug().
		Str("layer_digest", layerDigest).
		Str("decompressed_hash", decompressedHash).
		Msg("Stored layer digest -> decompressed hash mapping")
}

// Helper methods for tests (backward compatibility)

// getDiskCachePath returns cache path for a decompressed hash
// For tests: if we have a layer digest, look up its decompressed hash first
func (s *OCIClipStorage) getDiskCachePath(digestOrHash string) string {
	// Check if this is a layer digest that we have a mapping for
	if decompHash := s.getDecompressedHash(digestOrHash); decompHash != "" {
		return s.getDecompressedCachePath(decompHash)
	}
	// Otherwise treat it as already a hash
	return s.getDecompressedCachePath(digestOrHash)
}

// getContentHash for test compatibility - returns decompressed hash if known, otherwise empty
func (s *OCIClipStorage) getContentHash(layerDigest string) string {
	return s.getDecompressedHash(layerDigest)
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
// Returns the hash of the decompressed data
func (s *OCIClipStorage) decompressAndCacheLayer(digest string, diskPath string) (string, error) {
	metrics := common.GetGlobalMetrics()

	// Fetch from OCI registry and decompress
	s.mu.RLock()
	layer, exists := s.layerCache[digest]
	s.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("layer not found: %s", digest)
	}

	inflateStart := time.Now()

	// Fetch compressed layer from OCI registry
	compressedRC, err := layer.Compressed()
	if err != nil {
		return "", fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	// Create temp file for atomic write
	tempPath := diskPath + ".tmp"
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return "", fmt.Errorf("failed to create temp cache file: %w", err)
	}
	defer os.Remove(tempPath) // Clean up on error

	// Decompress directly to disk (streaming, low memory!)
	// Also compute hash of decompressed data as we write
	gzr, err := gzip.NewReader(compressedRC)
	if err != nil {
		tempFile.Close()
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Hash the decompressed data as we write it
	hasher := sha256.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, gzr)
	tempFile.Close()

	if err != nil {
		return "", fmt.Errorf("failed to decompress layer to disk: %w", err)
	}

	// Get the hash of the decompressed data
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	// Rename temp file to use decompressed hash as filename
	finalDiskPath := s.getDecompressedCachePath(decompressedHash)
	if err := os.Rename(tempPath, finalDiskPath); err != nil {
		return "", fmt.Errorf("failed to rename temp file: %w", err)
	}

	inflateDuration := time.Since(inflateStart)
	metrics.RecordInflateCPU(inflateDuration)

	log.Info().
		Str("layer_digest", digest).
		Str("decompressed_hash", decompressedHash).
		Int64("decompressed_bytes", written).
		Str("disk_path", finalDiskPath).
		Dur("duration", inflateDuration).
		Msg("Layer decompressed and cached to disk")

	// Store mapping from layer digest to decompressed hash
	s.storeDecompressedHashMapping(digest, decompressedHash)

	// Store in remote cache (if configured) for other workers
	if s.contentCache != nil {
		log.Info().
			Str("layer_digest", digest).
			Str("decompressed_hash", decompressedHash).
			Msg("Storing decompressed layer in ContentCache (async)")
		go s.storeDecompressedInRemoteCache(decompressedHash, finalDiskPath)
	} else {
		log.Warn().
			Str("layer_digest", digest).
			Msg("ContentCache not configured - layer will NOT be shared across cluster")
	}

	return decompressedHash, nil
}

// writeToDiskCache writes data to disk cache
func (s *OCIClipStorage) writeToDiskCache(path string, data []byte) error {
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// streamFileInChunks reads a file and sends it in chunks over a channel
// This matches the behavior in clipfs.go for consistent streaming
// Default chunk size is 32MB to balance memory usage and throughput
func streamFileInChunks(filePath string, chunks chan []byte) error {
	const chunkSize = int64(1 << 25) // 32MB chunks

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	fileSize := fileInfo.Size()

	// Stream in chunks
	for offset := int64(0); offset < fileSize; {
		// Calculate chunk size for this iteration
		currentChunkSize := chunkSize
		if remaining := fileSize - offset; remaining < chunkSize {
			currentChunkSize = remaining
		}

		// Read chunk
		buffer := make([]byte, currentChunkSize)
		nRead, err := io.ReadFull(file, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("failed to read chunk at offset %d: %w", offset, err)
		}

		// Send chunk
		if nRead > 0 {
			chunks <- buffer[:nRead]
		}

		offset += int64(nRead)
	}

	return nil
}

// tryRangeReadFromContentCache attempts a ranged read from remote ContentCache
// This enables lazy loading: we fetch only the bytes we need, not the entire layer
// decompressedHash is the hash of the decompressed layer data
func (s *OCIClipStorage) tryRangeReadFromContentCache(decompressedHash string, offset, length int64) ([]byte, error) {
	// Use GetContent for range reads (offset + length)
	// This is the KEY optimization: we only fetch the bytes we need!
	data, err := s.contentCache.GetContent(decompressedHash, offset, length, struct{ RoutingKey string }{})
	if err != nil {
		return nil, fmt.Errorf("ContentCache range read failed: %w", err)
	}

	return data, nil
}

// storeDecompressedInRemoteCache stores decompressed layer in remote cache (async safe)
// Stores the ENTIRE layer so other nodes can do range reads from it
// Streams content in chunks to avoid loading the entire layer into memory
// decompressedHash is the hash of the decompressed layer data (used as cache key)
func (s *OCIClipStorage) storeDecompressedInRemoteCache(decompressedHash string, diskPath string) {
	log.Debug().
		Str("decompressed_hash", decompressedHash).
		Str("disk_path", diskPath).
		Msg("storeDecompressedInRemoteCache goroutine started")

	// Get file size for logging
	fileInfo, err := os.Stat(diskPath)
	if err != nil {
		log.Error().
			Err(err).
			Str("decompressed_hash", decompressedHash).
			Str("disk_path", diskPath).
			Msg("FAILED to stat disk cache for ContentCache storage")
		return
	}
	totalSize := fileInfo.Size()

	// Stream the file in chunks (similar to clipfs.go)
	chunks := make(chan []byte, 1)

	go func() {
		defer close(chunks)

		if err := streamFileInChunks(diskPath, chunks); err != nil {
			log.Error().
				Err(err).
				Str("decompressed_hash", decompressedHash).
				Msg("FAILED to stream file for ContentCache storage")
		}
	}()

	storedHash, err := s.contentCache.StoreContent(chunks, decompressedHash, struct{ RoutingKey string }{})
	if err != nil {
		log.Error().
			Err(err).
			Str("decompressed_hash", decompressedHash).
			Int64("bytes", totalSize).
			Msg("FAILED to store layer in ContentCache")
	} else {
		log.Info().
			Str("decompressed_hash", decompressedHash).
			Str("stored_hash", storedHash).
			Int64("bytes", totalSize).
			Msg("✓ Successfully stored decompressed layer in ContentCache - available for cluster range reads")
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
