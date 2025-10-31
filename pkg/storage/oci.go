package storage

import (
	"compress/gzip"
	"context"
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
	credProvider          common.RegistryCredentialProvider // Credential provider for registry auth
	contentCache          ContentCache                      // Remote content cache (blobcache)
	contentCacheAvailable bool                              // is there an available content cache for range reads?
	useCheckpoints        bool                              // Enable checkpoint-based partial decompression
	mu                    sync.RWMutex
	layerDecompressMu     sync.Mutex               // Prevents duplicate decompression
	layersDecompressing   map[string]chan struct{} // Tracks in-progress decompressions
}

type OCIClipStorageOpts struct {
	Metadata              *common.ClipArchiveMetadata
	CredProvider          common.RegistryCredentialProvider // optional credential provider for registry authentication
	ContentCache          ContentCache                      // optional remote content cache (blobcache)
	ContentCacheAvailable bool                              // is there an available content cache for range reads?
	DiskCacheDir          string                            // optional local disk cache directory
	UseCheckpoints        bool                              // Enable checkpoint-based partial decompression (default: false)
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

	// Determine which credential provider to use
	credProvider := opts.CredProvider
	if credProvider == nil {
		credProvider = common.DefaultProvider()
	}

	storage := &OCIClipStorage{
		metadata:              opts.Metadata,
		storageInfo:           &storageInfo,
		layerCache:            make(map[string]v1.Layer),
		diskCacheDir:          diskCacheDir,
		httpClient:            &http.Client{},
		credProvider:          credProvider,
		contentCache:          opts.ContentCache,
		contentCacheAvailable: opts.ContentCacheAvailable,
		useCheckpoints:        opts.UseCheckpoints,
		layersDecompressing:   make(map[string]chan struct{}),
	}

	log.Info().
		Str("cache_dir", diskCacheDir).
		Str("cred_provider", credProvider.Name()).
		Msg("initialized OCI storage with disk cache")

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

	// Build remote options with authentication
	remoteOpts := []remote.Option{remote.WithContext(ctx)}

	// Try to get credentials from provider
	authConfig, err := s.credProvider.GetCredentials(ctx, s.storageInfo.RegistryURL, s.storageInfo.Repository)
	if err != nil && err != common.ErrNoCredentials {
		log.Warn().
			Err(err).
			Str("registry", s.storageInfo.RegistryURL).
			Str("provider", s.credProvider.Name()).
			Msg("Failed to get credentials from provider, falling back to keychain")
	}

	if authConfig != nil {
		// Use provided credentials
		log.Debug().
			Str("registry", s.storageInfo.RegistryURL).
			Str("provider", s.credProvider.Name()).
			Msg("Using credentials from provider for layer init")
		// Convert AuthConfig to proper authenticator (handles all auth types: username/password, tokens, etc.)
		auth := authn.FromConfig(*authConfig)
		remoteOpts = append(remoteOpts, remote.WithAuth(auth))
	} else {
		// Fall back to default keychain for anonymous or keychain-based auth
		log.Debug().
			Str("registry", s.storageInfo.RegistryURL).
			Msg("No credentials from provider for layer init, using default keychain")
		remoteOpts = append(remoteOpts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}

	img, err := remote.Image(ref, remoteOpts...)
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
//  3. Decompress from OCI - with checkpoints if enabled, otherwise full layer
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

	// Try disk cache first
	if decompressedHash != "" {
		layerPath := s.getDecompressedCachePath(decompressedHash)
		if _, err := os.Stat(layerPath); err == nil {
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Msg("disk cache hit - using local decompressed layer")
			return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
		}
	}

	// Try remote ContentCache range read
	if s.contentCache != nil && decompressedHash != "" && s.contentCacheAvailable {
		if data, err := s.tryRangeReadFromContentCache(decompressedHash, wantUStart, readLen); err == nil {
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Int("bytes_read", len(data)).
				Msg("content cache hit - range read from remote")
			copy(dest, data)
			return len(data), nil
		} else {
			log.Debug().
				Err(err).
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Msg("content cache miss - will decompress from OCI")
		}
	}

	// Cache miss - try checkpoint-based decompression if enabled
	if s.useCheckpoints {
		if n, err := s.readWithCheckpoint(remote.LayerDigest, wantUStart, dest[:readLen]); err == nil {
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Int("bytes_read", n).
				Msg("checkpoint-based decompression successful")
			return n, nil
		} else {
			log.Debug().
				Err(err).
				Str("layer_digest", remote.LayerDigest).
				Msg("checkpoint-based decompression failed, falling back to full layer decompression")
		}
	}

	// Fallback: decompress entire layer and cache (for future range reads)
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
	// Get pre-computed decompressed hash from metadata
	decompressedHash := s.getDecompressedHash(digest)
	if decompressedHash == "" {
		return "", "", fmt.Errorf("no decompressed hash in metadata for layer: %s", digest)
	}

	layerPath := s.getDecompressedCachePath(decompressedHash)

	// Check if already cached on disk
	if _, err := os.Stat(layerPath); err == nil {
		log.Debug().Str("digest", digest).Str("decompressed_hash", decompressedHash).Msg("disk cache hit")
		return decompressedHash, layerPath, nil
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
			return decompressedHash, layerPath, nil
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
		Str("decompressed_hash", decompressedHash).
		Msg("oci cache miss - downloading and decompressing layer from registry")

	err := s.decompressAndCacheLayer(digest, layerPath)

	// Clean up in-progress tracking
	s.layerDecompressMu.Lock()
	delete(s.layersDecompressing, digest)
	close(doneChan)
	s.layerDecompressMu.Unlock()

	if err != nil {
		return "", "", err
	}

	return decompressedHash, layerPath, nil
}

// getDecompressedCachePath returns the cache path for a decompressed hash
func (s *OCIClipStorage) getDecompressedCachePath(decompressedHash string) string {
	return filepath.Join(s.diskCacheDir, decompressedHash)
}

// getDecompressedHash retrieves the pre-computed decompressed hash for a layer digest from metadata
func (s *OCIClipStorage) getDecompressedHash(layerDigest string) string {
	if s.storageInfo.DecompressedHashByLayer == nil {
		return ""
	}
	return s.storageInfo.DecompressedHashByLayer[layerDigest]
}

// getDiskCachePath returns cache path for a layer digest (looks up decompressed hash from metadata)
func (s *OCIClipStorage) getDiskCachePath(layerDigest string) string {
	decompHash := s.getDecompressedHash(layerDigest)
	if decompHash != "" {
		return s.getDecompressedCachePath(decompHash)
	}

	// Fallback for tests without metadata
	return s.getDecompressedCachePath(layerDigest)
}

// getContentHash for test compatibility - returns decompressed hash from metadata
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
func (s *OCIClipStorage) decompressAndCacheLayer(digest string, diskPath string) error {
	metrics := common.GetGlobalMetrics()

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
		Str("layer_digest", digest).
		Int64("decompressed_bytes", written).
		Str("disk_path", diskPath).
		Dur("duration", inflateDuration).
		Msg("Layer decompressed and cached to disk")

	// Store in remote cache (if configured) for other workers
	if s.contentCache != nil {
		decompressedHash := s.getDecompressedHash(digest)
		log.Info().
			Str("layer_digest", digest).
			Str("decompressed_hash", decompressedHash).
			Msg("storing decompressed layer in content cache")
		go s.storeDecompressedInRemoteCache(decompressedHash, diskPath)
	} else {
		log.Warn().
			Str("layer_digest", digest).
			Msg("content cache not configured - layer will NOT be shared across cluster")
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
		return nil, fmt.Errorf("content cache range read failed: %w", err)
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
			Msg("failed to stat disk cache for content cache storage")
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
				Msg("failed to stream file for content cache storage")
		}
	}()

	storedHash, err := s.contentCache.StoreContent(chunks, decompressedHash, struct{ RoutingKey string }{})
	if err != nil {
		log.Error().
			Err(err).
			Str("decompressed_hash", decompressedHash).
			Int64("bytes", totalSize).
			Msg("failed to store layer in content cache")
	} else {
		log.Info().
			Str("decompressed_hash", decompressedHash).
			Str("stored_hash", storedHash).
			Int64("bytes", totalSize).
			Msg("successfully stored decompressed layer in content cache")
	}
}

// readWithCheckpoint reads data from a compressed layer using gzip checkpoints
// This enables efficient random access without decompressing the entire layer
func (s *OCIClipStorage) readWithCheckpoint(layerDigest string, wantUOffset int64, dest []byte) (int, error) {
	// Get gzip index for this layer
	gzipIndex, ok := s.storageInfo.GzipIdxByLayer[layerDigest]
	if !ok || gzipIndex == nil || len(gzipIndex.Checkpoints) == 0 {
		return 0, fmt.Errorf("no gzip checkpoints available for layer: %s", layerDigest)
	}

	// Find the nearest checkpoint
	cOff, uOff := common.NearestCheckpoint(gzipIndex.Checkpoints, wantUOffset)

	log.Debug().
		Str("layer_digest", layerDigest).
		Int64("want_uoffset", wantUOffset).
		Int64("checkpoint_coff", cOff).
		Int64("checkpoint_uoff", uOff).
		Int64("decompress_bytes", wantUOffset-uOff+int64(len(dest))).
		Msg("using checkpoint for partial decompression")

	// Get layer from cache
	s.mu.RLock()
	layer, exists := s.layerCache[layerDigest]
	s.mu.RUnlock()

	if !exists {
		return 0, fmt.Errorf("layer not found: %s", layerDigest)
	}

	// Fetch compressed layer stream
	compressedRC, err := layer.Compressed()
	if err != nil {
		return 0, fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	// Seek to checkpoint's compressed offset
	// Note: We need a seekable reader for this. If the reader doesn't support seeking,
	// we'll need to discard bytes up to the checkpoint
	if cOff > 0 {
		// Discard bytes up to checkpoint
		_, err := io.CopyN(io.Discard, compressedRC, cOff)
		if err != nil {
			return 0, fmt.Errorf("failed to seek to checkpoint compressed offset: %w", err)
		}
	}

	// Create gzip reader starting from checkpoint
	gzr, err := gzip.NewReader(compressedRC)
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Skip bytes in uncompressed stream from checkpoint to desired offset
	skipBytes := wantUOffset - uOff
	if skipBytes > 0 {
		_, err := io.CopyN(io.Discard, gzr, skipBytes)
		if err != nil {
			return 0, fmt.Errorf("failed to skip to desired uncompressed offset: %w", err)
		}
	}

	// Read the requested data
	n, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return n, fmt.Errorf("failed to read from gzip stream: %w", err)
	}

	return n, nil
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
