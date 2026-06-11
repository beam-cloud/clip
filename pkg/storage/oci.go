package storage

import (
	// klauspost/compress gunzip is substantially faster than stdlib for
	// full-layer decompression on cache misses
	"github.com/klauspost/compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	readTraceObserver     common.ReadTraceObserver
	mu                    sync.RWMutex
	contentCacheWarmMu    sync.Mutex
	contentCacheWarmOnce  map[string]struct{}
	contentCacheReadAhead *ContentCacheReadAhead
	layerLimitByHash      map[string]int64
}

var globalLayerDecompress = newLayerDecompressGroup()

type layerDecompressGroup struct {
	mu       sync.Mutex
	inflight map[string]*layerDecompressCall
}

type layerDecompressCall struct {
	done chan struct{}
	err  error
}

func newLayerDecompressGroup() *layerDecompressGroup {
	return &layerDecompressGroup{inflight: make(map[string]*layerDecompressCall)}
}

func (g *layerDecompressGroup) Do(key string, fn func() error) (shared bool, err error) {
	g.mu.Lock()
	if call := g.inflight[key]; call != nil {
		g.mu.Unlock()
		<-call.done
		return true, call.err
	}

	call := &layerDecompressCall{done: make(chan struct{})}
	g.inflight[key] = call
	g.mu.Unlock()

	call.err = fn()

	g.mu.Lock()
	delete(g.inflight, key)
	close(call.done)
	g.mu.Unlock()

	return false, call.err
}

type OCIClipStorageOpts struct {
	Metadata              *common.ClipArchiveMetadata
	CredProvider          common.RegistryCredentialProvider // optional credential provider for registry authentication
	ContentCache          ContentCache                      // optional remote content cache (blobcache)
	ContentCacheAvailable bool                              // is there an available content cache for range reads?
	DiskCacheDir          string                            // optional local disk cache directory
	UseCheckpoints        bool                              // Enable checkpoint-based partial decompression (default: false)
	ReadTraceObserver     common.ReadTraceObserver
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
		readTraceObserver:     opts.ReadTraceObserver,
		contentCacheWarmOnce:  make(map[string]struct{}),
		contentCacheReadAhead: NewContentCacheReadAhead(opts.ContentCache, ContentCacheReadAheadOptions{}),
		layerLimitByHash:      ociLayerLimitsByHash(opts.Metadata, &storageInfo),
	}

	log.Info().
		Str("cache_dir", diskCacheDir).
		Str("cred_provider", credProvider.Name()).
		Bool("content_cache_available", opts.ContentCache != nil && opts.ContentCacheAvailable).
		Msg("initialized OCI storage with disk cache")

	return storage, nil
}

// initLayers fetches layer descriptors from the registry
func (s *OCIClipStorage) initLayers(ctx context.Context) error {
	imageRef := s.imageReference()

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("failed to parse image reference: %w", err)
	}

	remoteOpts := s.remoteOptions(ctx)
	platform := v1.Platform{
		OS:           "linux",
		Architecture: runtime.GOARCH,
	}
	remoteOpts = append(remoteOpts, remote.WithPlatform(platform))

	log.Debug().
		Str("image_ref", imageRef).
		Str("platform", platform.Architecture).
		Msg("fetching image layers from registry")

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

func (s *OCIClipStorage) imageReference() string {
	if strings.HasPrefix(s.storageInfo.Reference, "sha256:") {
		return fmt.Sprintf("%s/%s@%s", s.storageInfo.RegistryURL, s.storageInfo.Repository, s.storageInfo.Reference)
	}
	return fmt.Sprintf("%s/%s:%s", s.storageInfo.RegistryURL, s.storageInfo.Repository, s.storageInfo.Reference)
}

func (s *OCIClipStorage) remoteOptions(ctx context.Context) []remote.Option {
	remoteOpts := []remote.Option{remote.WithContext(ctx)}

	authConfig, err := s.credProvider.GetCredentials(ctx, s.storageInfo.RegistryURL, s.storageInfo.Repository)
	if err != nil && err != common.ErrNoCredentials {
		log.Warn().
			Err(err).
			Str("registry", s.storageInfo.RegistryURL).
			Str("repository", s.storageInfo.Repository).
			Str("provider", s.credProvider.Name()).
			Msg("Failed to get credentials from provider, falling back to keychain")
	}

	if authConfig != nil {
		log.Info().
			Str("registry", s.storageInfo.RegistryURL).
			Str("repository", s.storageInfo.Repository).
			Str("provider", s.credProvider.Name()).
			Bool("has_username", authConfig.Username != "").
			Bool("has_password", authConfig.Password != "").
			Bool("has_auth", authConfig.Auth != "").
			Bool("has_identity_token", authConfig.IdentityToken != "").
			Bool("has_registry_token", authConfig.RegistryToken != "").
			Msg("Using credentials from provider for layer init")
		remoteOpts = append(remoteOpts, remote.WithAuth(authn.FromConfig(*authConfig)))
	} else {
		log.Warn().
			Err(err).
			Str("registry", s.storageInfo.RegistryURL).
			Str("repository", s.storageInfo.Repository).
			Str("provider", s.credProvider.Name()).
			Msg("No credentials from provider for layer init, using default keychain")
		remoteOpts = append(remoteOpts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}

	return remoteOpts
}

func (s *OCIClipStorage) fetchLayerByDigest(ctx context.Context, digest string) (v1.Layer, error) {
	layerRef := fmt.Sprintf("%s/%s@%s", s.storageInfo.RegistryURL, s.storageInfo.Repository, digest)
	ref, err := name.NewDigest(layerRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse layer digest reference %q: %w", layerRef, err)
	}
	layer, err := remote.Layer(ref, s.remoteOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch layer by digest %s: %w", digest, err)
	}
	return layer, nil
}

// ReadFile reads file content using ranged reads from disk or remote cache
//  1. Check disk cache (range read) - fastest, local
//  2. Check ContentCache (range read) - fast, network but only what we need
//  3. Decompress from OCI - with checkpoints if enabled, otherwise full layer
func (s *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	return s.ReadFileContext(context.Background(), node, dest, offset)
}

func (s *OCIClipStorage) ClientLocalFileView(ctx context.Context, node *common.ClipNode, offset int64, length int64) (ClientLocalFileView, bool, error) {
	if node == nil || node.Remote == nil || length <= 0 || offset < 0 {
		return ClientLocalFileView{}, false, nil
	}

	remote := node.Remote
	wantUStart := remote.UOffset + offset
	wantUEnd := remote.UOffset + remote.ULength
	readLen := length
	if wantUStart+readLen > wantUEnd {
		readLen = wantUEnd - wantUStart
	}
	if readLen <= 0 || readLen > int64(int(^uint(0)>>1)) {
		return ClientLocalFileView{}, false, nil
	}

	decompressedHash := s.getDecompressedHash(remote.LayerDigest)
	if decompressedHash == "" {
		return ClientLocalFileView{}, false, nil
	}

	layerPath := s.getDecompressedCachePath(decompressedHash)
	if _, err := os.Stat(layerPath); err == nil {
		warmDecision := s.scheduleDecompressedLayerContentCacheWarm(decompressedHash, layerPath)
		return ClientLocalFileView{
			Path:             layerPath,
			Offset:           wantUStart,
			Length:           int(readLen),
			Source:           "disk_cache_fd",
			LayerDigest:      remote.LayerDigest,
			DecompressedHash: decompressedHash,
			Attrs: map[string]string{
				"cache_result":            "hit",
				"cache_tier":              "local_decompressed_layer",
				"content_cache_available": fmt.Sprintf("%t", s.contentCacheAvailable),
				"content_cache_warm":      warmDecision,
				"storage_mode":            "oci",
			},
		}, true, nil
	} else if !os.IsNotExist(err) {
		return ClientLocalFileView{}, false, err
	}

	pageCache, ok := s.contentCache.(ContentCacheClientLocalPageFileViews)
	if !ok || pageCache == nil || !s.contentCacheAvailable {
		return ClientLocalFileView{}, false, nil
	}

	views, err := pageCache.ClientLocalPageFileViews(decompressedHash, wantUStart, readLen, struct{ RoutingKey string }{RoutingKey: decompressedHash})
	if err != nil || len(views) != 1 {
		return ClientLocalFileView{}, false, err
	}
	view := views[0]
	if view.Path == "" || view.Offset < 0 || view.Length != int(readLen) {
		return ClientLocalFileView{}, false, nil
	}

	return ClientLocalFileView{
		Path:             view.Path,
		Offset:           view.Offset,
		Length:           view.Length,
		Source:           "client_local_page_file_fd",
		LayerDigest:      remote.LayerDigest,
		DecompressedHash: decompressedHash,
		Attrs: map[string]string{
			"cache_result":            "hit",
			"cache_tier":              "embedded_page_file",
			"content_cache_available": fmt.Sprintf("%t", s.contentCacheAvailable),
			"storage_mode":            "oci",
		},
	}, true, nil
}

func (s *OCIClipStorage) ReadFileContext(ctx context.Context, node *common.ClipNode, dest []byte, offset int64) (n int, err error) {
	if node.Remote == nil {
		return 0, fmt.Errorf("legacy data storage not supported in OCI mode")
	}

	remote := node.Remote
	readStart := time.Now()
	readSource := "unknown"
	readAttrs := map[string]string{
		"content_cache_available": fmt.Sprintf("%t", s.contentCacheAvailable),
		"storage_mode":            "oci",
	}

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
	defer func() {
		s.observeRead(ctx, common.ReadTraceEvent{
			Operation:        "clip.read",
			Source:           readSource,
			Path:             node.Path,
			LayerDigest:      remote.LayerDigest,
			DecompressedHash: s.getDecompressedHash(remote.LayerDigest),
			Offset:           wantUStart,
			Length:           readLen,
			BytesRead:        int64(n),
			StartedAt:        readStart,
			Duration:         time.Since(readStart),
			Success:          err == nil,
			Error:            errorString(err),
			Attrs:            readAttrs,
		})
	}()

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
			warmDecision := s.scheduleDecompressedLayerContentCacheWarm(decompressedHash, layerPath)
			metrics.RecordReadHit()
			readSource = "disk_cache"
			readAttrs["cache_result"] = "hit"
			readAttrs["cache_tier"] = "local_decompressed_layer"
			readAttrs["content_cache_warm"] = warmDecision
			return s.readFromDiskCacheObserved(ctx, node.Path, remote.LayerDigest, decompressedHash, layerPath, wantUStart, dest[:readLen])
		}
	}

	// Try remote ContentCache range read
	if s.contentCache != nil && decompressedHash != "" && s.contentCacheAvailable {
		cacheStart := time.Now()
		if n, err := s.tryRangeReadFromContentCache(decompressedHash, wantUStart, dest[:readLen], s.contentCacheReadLimit(decompressedHash, remote)); err == nil {
			metrics.RecordReadHit()
			metrics.RecordRangeGet(decompressedHash, int64(n))
			readSource = "content_cache"
			s.observeRead(ctx, common.ReadTraceEvent{
				Operation:        "clip.content_cache_read",
				Source:           "content_cache",
				Path:             node.Path,
				LayerDigest:      remote.LayerDigest,
				DecompressedHash: decompressedHash,
				Offset:           wantUStart,
				Length:           readLen,
				BytesRead:        int64(n),
				StartedAt:        cacheStart,
				Duration:         time.Since(cacheStart),
				Success:          true,
				Attrs: map[string]string{
					"cache_result":            "hit",
					"cache_tier":              "embedded_content_cache",
					"content_cache_available": fmt.Sprintf("%t", s.contentCacheAvailable),
					"storage_mode":            "oci",
				},
			})
			readAttrs["cache_result"] = "hit"
			readAttrs["cache_tier"] = "embedded_content_cache"
			readAttrs["content_cache_result"] = "hit"
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Int("bytes_read", n).
				Msg("content cache hit - range read from remote")
			return n, nil
		} else {
			metrics.RecordReadMiss()
			s.observeRead(ctx, common.ReadTraceEvent{
				Operation:        "clip.content_cache_read",
				Source:           "content_cache",
				Path:             node.Path,
				LayerDigest:      remote.LayerDigest,
				DecompressedHash: decompressedHash,
				Offset:           wantUStart,
				Length:           readLen,
				StartedAt:        cacheStart,
				Duration:         time.Since(cacheStart),
				Success:          false,
				Error:            errorString(err),
				Attrs: map[string]string{
					"cache_result":            contentCacheErrorKind(err),
					"cache_tier":              "embedded_content_cache",
					"content_cache_available": fmt.Sprintf("%t", s.contentCacheAvailable),
					"storage_mode":            "oci",
				},
			})
			readAttrs["content_cache_result"] = contentCacheErrorKind(err)
			log.Debug().
				Err(err).
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Msg("content cache range read failed")
			if errors.Is(err, ErrContentCacheUnavailable) {
				log.Warn().
					Err(err).
					Str("layer_digest", remote.LayerDigest).
					Str("decompressed_hash", decompressedHash).
					Msg("content cache unavailable - falling back to OCI for correctness")
			}
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Str("decompressed_hash", decompressedHash).
				Msg("content cache miss - will decompress from OCI")
		}
	}

	// Cache miss - try checkpoint-based decompression if enabled
	if s.useCheckpoints {
		checkpointStart := time.Now()
		if n, err := s.readWithCheckpoint(remote.LayerDigest, wantUStart, dest[:readLen]); err == nil {
			readSource = "checkpoint"
			readAttrs["cache_result"] = "miss"
			readAttrs["cache_tier"] = "checkpoint"
			readAttrs["fallback"] = "checkpoint"
			s.observeRead(ctx, common.ReadTraceEvent{
				Operation:   "clip.checkpoint_read",
				Source:      "checkpoint",
				Path:        node.Path,
				LayerDigest: remote.LayerDigest,
				Offset:      wantUStart,
				Length:      readLen,
				BytesRead:   int64(n),
				StartedAt:   checkpointStart,
				Duration:    time.Since(checkpointStart),
				Success:     true,
				Attrs: map[string]string{
					"cache_result": "miss",
					"cache_tier":   "checkpoint",
					"fallback":     "checkpoint",
					"storage_mode": "oci",
				},
			})
			log.Debug().
				Str("layer_digest", remote.LayerDigest).
				Int64("offset", wantUStart).
				Int64("length", readLen).
				Int("bytes_read", n).
				Msg("checkpoint-based decompression successful")
			if s.contentCache != nil && s.contentCacheAvailable && decompressedHash != "" {
				if _, _, cacheErr := s.ensureLayerCached(ctx, remote.LayerDigest); cacheErr != nil {
					log.Warn().
						Err(cacheErr).
						Str("layer_digest", remote.LayerDigest).
						Str("decompressed_hash", decompressedHash).
						Msg("failed to materialize decompressed layer after checkpoint read")
				}
			}
			return n, nil
		} else {
			s.observeRead(ctx, common.ReadTraceEvent{
				Operation:   "clip.checkpoint_read",
				Source:      "checkpoint",
				Path:        node.Path,
				LayerDigest: remote.LayerDigest,
				Offset:      wantUStart,
				Length:      readLen,
				StartedAt:   checkpointStart,
				Duration:    time.Since(checkpointStart),
				Success:     false,
				Error:       errorString(err),
				Attrs: map[string]string{
					"cache_result": "miss",
					"cache_tier":   "checkpoint",
					"fallback":     "oci_decompress",
					"storage_mode": "oci",
				},
			})
			log.Debug().
				Err(err).
				Str("layer_digest", remote.LayerDigest).
				Msg("checkpoint-based decompression failed, falling back to full layer decompression")
		}
	}

	// Fallback: decompress entire layer and cache (for future range reads)
	decompressedHash, layerPath, err := s.ensureLayerCached(ctx, remote.LayerDigest)
	if err != nil {
		return 0, err
	}

	// Now read the range we need from the newly cached layer
	readSource = "decompressed_layer"
	readAttrs["cache_result"] = "miss"
	readAttrs["cache_tier"] = "oci_decompressed_layer"
	readAttrs["fallback"] = "oci_decompress"
	return s.readFromDiskCacheObserved(ctx, node.Path, remote.LayerDigest, decompressedHash, layerPath, wantUStart, dest[:readLen])
}

// ensureLayerCached ensures the decompressed layer is available on disk
// Returns decompressed hash and path
func (s *OCIClipStorage) ensureLayerCached(ctx context.Context, digest string) (string, string, error) {
	// Get pre-computed decompressed hash from metadata
	decompressedHash := s.getDecompressedHash(digest)
	if decompressedHash == "" {
		return "", "", fmt.Errorf("no decompressed hash in metadata for layer: %s", digest)
	}

	layerPath := s.getDecompressedCachePath(decompressedHash)

	// Fast path: check if already cached on disk (outside lock for performance)
	if _, err := os.Stat(layerPath); err == nil {
		log.Debug().Str("digest", digest).Str("decompressed_hash", decompressedHash).Msg("disk cache hit")
		s.scheduleDecompressedLayerContentCacheWarm(decompressedHash, layerPath)
		return decompressedHash, layerPath, nil
	}

	waitStart := time.Now()
	shared, err := globalLayerDecompress.Do(decompressedHash, func() error {
		// Double-check disk cache inside the process-wide singleflight. A
		// separate OCIClipStorage instance may have materialized the same layer
		// between our fast-path stat and entering this call.
		if _, err := os.Stat(layerPath); err == nil {
			log.Debug().Str("digest", digest).Str("decompressed_hash", decompressedHash).Msg("disk cache hit (after global lock)")
			s.scheduleDecompressedLayerContentCacheWarm(decompressedHash, layerPath)
			return nil
		}

		log.Info().
			Str("layer_digest", digest).
			Str("decompressed_hash", decompressedHash).
			Msg("oci cache miss - downloading and decompressing layer from registry")

		decompressStart := time.Now()
		err := s.decompressAndCacheLayer(digest, layerPath)
		s.observeRead(ctx, common.ReadTraceEvent{
			Operation:        "clip.layer_decompress",
			Source:           "oci_registry",
			LayerDigest:      digest,
			DecompressedHash: decompressedHash,
			StartedAt:        decompressStart,
			Duration:         time.Since(decompressStart),
			Success:          err == nil,
			Error:            errorString(err),
		})
		return err
	})
	if shared {
		log.Info().Str("digest", digest).Msg("waited for in-progress layer decompression")
		s.observeRead(ctx, common.ReadTraceEvent{
			Operation:        "clip.layer_decompress_wait",
			Source:           "decompressed_layer",
			LayerDigest:      digest,
			DecompressedHash: decompressedHash,
			StartedAt:        waitStart,
			Duration:         time.Since(waitStart),
			Success:          err == nil,
			Error:            errorString(err),
		})
	}

	if err != nil {
		log.Error().Err(err).Str("digest", digest).Msg("layer decompression failed")
		return "", "", err
	}

	if _, err := os.Stat(layerPath); err != nil {
		return "", "", fmt.Errorf("decompression did not materialize layer %s at %s: %w", digest, layerPath, err)
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

func (s *OCIClipStorage) readFromDiskCacheObserved(ctx context.Context, path, layerDigest, decompressedHash, layerPath string, offset int64, dest []byte) (int, error) {
	startedAt := time.Now()
	n, err := s.readFromDiskCache(layerPath, offset, dest)
	s.observeRead(ctx, common.ReadTraceEvent{
		Operation:        "clip.disk_cache_read",
		Source:           "disk_cache",
		Path:             path,
		LayerDigest:      layerDigest,
		DecompressedHash: decompressedHash,
		Offset:           offset,
		Length:           int64(len(dest)),
		BytesRead:        int64(n),
		StartedAt:        startedAt,
		Duration:         time.Since(startedAt),
		Success:          err == nil,
		Error:            errorString(err),
	})
	return n, err
}

func (s *OCIClipStorage) observeRead(ctx context.Context, event common.ReadTraceEvent) {
	if s.readTraceObserver == nil {
		return
	}
	if event.StartedAt.IsZero() {
		event.StartedAt = time.Now().Add(-event.Duration)
	}
	if event.CallerPID == 0 {
		event.CallerPID = common.ReadTraceCallerPID(ctx)
	}
	s.readTraceObserver(event)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *OCIClipStorage) warmDecompressedLayerInContentCache(decompressedHash string, diskPath string) {
	if s.contentCache == nil || !s.contentCacheAvailable || decompressedHash == "" || diskPath == "" {
		return
	}
	if err := s.storeDecompressedInRemoteCache(decompressedHash, diskPath); err != nil {
		log.Warn().
			Err(err).
			Str("decompressed_hash", decompressedHash).
			Str("path", diskPath).
			Msg("content cache store failed while warming decompressed layer")
	}
}

func (s *OCIClipStorage) scheduleDecompressedLayerContentCacheWarm(decompressedHash string, diskPath string) string {
	if s.contentCache == nil {
		return "disabled_no_cache"
	}
	if !s.contentCacheAvailable {
		return "disabled_unavailable"
	}
	if decompressedHash == "" || diskPath == "" {
		return "skipped_invalid_source"
	}
	if !s.markContentCacheWarmAttempt(decompressedHash) {
		return "already_attempted"
	}

	go s.warmDecompressedLayerInContentCache(decompressedHash, diskPath)
	return "scheduled"
}

func (s *OCIClipStorage) markContentCacheWarmAttempt(decompressedHash string) bool {
	s.contentCacheWarmMu.Lock()
	defer s.contentCacheWarmMu.Unlock()

	if s.contentCacheWarmOnce == nil {
		s.contentCacheWarmOnce = make(map[string]struct{})
	}
	if _, ok := s.contentCacheWarmOnce[decompressedHash]; ok {
		return false
	}
	s.contentCacheWarmOnce[decompressedHash] = struct{}{}
	return true
}

func contentCacheErrorKind(err error) string {
	switch {
	case err == nil:
		return "hit"
	case errors.Is(err, ErrContentCacheMiss):
		return "miss"
	case errors.Is(err, ErrContentCacheUnavailable):
		return "unavailable"
	default:
		return "error"
	}
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
		fetched, err := s.fetchLayerByDigest(context.Background(), digest)
		if err != nil {
			return fmt.Errorf("layer not found: %s: %w", digest, err)
		}
		layer = fetched
		s.mu.Lock()
		s.layerCache[digest] = layer
		s.mu.Unlock()
	}

	inflateStart := time.Now()

	// Fetch compressed layer from OCI registry
	compressedRC, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	// Create a unique same-directory temp file. Multiple worker processes can
	// share the same disk cache directory, so a deterministic "<hash>.tmp" name
	// can race even though each process has its own singleflight.
	tempFile, err := os.CreateTemp(filepath.Dir(diskPath), filepath.Base(diskPath)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp cache file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath) // Clean up on error or if another process wins.

	// Decompress directly to disk (streaming)
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

	decompressedHash := s.getDecompressedHash(digest)
	if _, err := os.Stat(diskPath); err == nil {
		s.scheduleDecompressedLayerContentCacheWarm(decompressedHash, diskPath)
		return nil
	}

	// Atomic rename. If another worker materialized the same immutable layer
	// between our final stat and rename, treat that as success.
	if err := os.Rename(tempPath, diskPath); err != nil {
		if _, statErr := os.Stat(diskPath); statErr == nil {
			s.scheduleDecompressedLayerContentCacheWarm(decompressedHash, diskPath)
			return nil
		}
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	duration := time.Since(inflateStart)
	metrics.RecordInflateCPU(duration)

	log.Info().
		Str("layer", digest).
		Int64("bytes", written).
		Dur("duration", duration).
		Msg("layer decompressed and cached")

	// Publish to the shared content cache before returning. This keeps the
	// "layer cached" state durable across worker replacement; otherwise a short
	// workload can finish, the worker can be deleted, and the background store
	// can be lost before the next worker tries to reuse the layer.
	if s.contentCache != nil && s.contentCacheAvailable {
		if err := s.storeDecompressedInRemoteCache(decompressedHash, diskPath); err != nil {
			log.Warn().
				Err(err).
				Str("layer", digest).
				Str("decompressed_hash", decompressedHash).
				Msg("content cache store failed after layer decompression")
		}
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
	return streamFileInChunksUntil(filePath, chunks, nil)
}

func streamFileInChunksUntil(filePath string, chunks chan []byte, done <-chan struct{}) error {
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
			if done == nil {
				chunks <- buffer[:nRead]
			} else {
				select {
				case chunks <- buffer[:nRead]:
				case <-done:
					return nil
				}
			}
		}

		offset += int64(nRead)
	}

	return nil
}

// tryRangeReadFromContentCache attempts a ranged read from remote ContentCache
// This enables lazy loading: we fetch only the bytes we need, not the entire layer
// decompressedHash is the hash of the decompressed layer data
func (s *OCIClipStorage) tryRangeReadFromContentCache(decompressedHash string, offset int64, dest []byte, limit int64) (int, error) {
	// Defensive nil check (should already be checked by caller)
	if s.contentCache == nil {
		return 0, fmt.Errorf("content cache is not available")
	}

	length := int64(len(dest))
	if readAhead := s.getContentCacheReadAhead(); readAhead != nil {
		n, err := readAhead.Read(decompressedHash, offset, dest, struct{ RoutingKey string }{RoutingKey: decompressedHash}, limit)
		if err != nil {
			return 0, fmt.Errorf("content cache range read failed: %w", err)
		}
		if n != length {
			return 0, fmt.Errorf("content cache short read: want %d, got %d", length, n)
		}
		return int(n), nil
	}

	n, err := readContentCacheInto(s.contentCache, decompressedHash, offset, dest, struct{ RoutingKey string }{RoutingKey: decompressedHash})
	if err != nil {
		return 0, fmt.Errorf("content cache range read failed: %w", err)
	}
	if n != length {
		return 0, fmt.Errorf("content cache short read: want %d, got %d", length, n)
	}
	return int(n), nil
}

func (s *OCIClipStorage) getContentCacheReadAhead() *ContentCacheReadAhead {
	if s == nil || s.contentCache == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contentCacheReadAhead == nil {
		s.contentCacheReadAhead = NewContentCacheReadAhead(s.contentCache, ContentCacheReadAheadOptions{})
	}
	return s.contentCacheReadAhead
}

func (s *OCIClipStorage) contentCacheReadLimit(decompressedHash string, remote *common.RemoteRef) int64 {
	if s == nil || decompressedHash == "" {
		return 0
	}
	s.mu.Lock()
	if s.layerLimitByHash == nil {
		s.layerLimitByHash = ociLayerLimitsByHash(s.metadata, s.storageInfo)
	}
	limit := s.layerLimitByHash[decompressedHash]
	s.mu.Unlock()
	if limit > 0 {
		return limit
	}
	if remote != nil && remote.UOffset >= 0 && remote.ULength > 0 {
		return remote.UOffset + remote.ULength
	}
	return 0
}

func ociLayerLimitsByHash(metadata *common.ClipArchiveMetadata, storageInfo *common.OCIStorageInfo) map[string]int64 {
	if metadata == nil || metadata.Index == nil || storageInfo == nil {
		return nil
	}
	limits := make(map[string]int64)
	metadata.Index.Ascend(nil, func(item interface{}) bool {
		node, ok := item.(*common.ClipNode)
		if !ok || node == nil || node.Remote == nil {
			return true
		}
		hash := storageInfo.DecompressedHashByLayer[node.Remote.LayerDigest]
		if hash == "" {
			return true
		}
		end := node.Remote.UOffset + node.Remote.ULength
		if end > limits[hash] {
			limits[hash] = end
		}
		return true
	})
	return limits
}

// storeDecompressedInRemoteCache uploads decompressed layer to remote cache for cluster sharing.
// Streams file in 32MB chunks with constant memory usage O(32MB).
func (s *OCIClipStorage) storeDecompressedInRemoteCache(decompressedHash string, diskPath string) error {
	if decompressedHash == "" {
		return fmt.Errorf("decompressed hash is required for content cache store")
	}

	// Guard against nil contentCache or unavailable cache
	if s.contentCache == nil {
		log.Debug().
			Str("hash", decompressedHash).
			Bool("cache_nil", true).
			Msg("skipping remote cache store - cache not available")
		return nil
	}

	if existsCache, ok := s.contentCache.(ContentCacheExists); ok {
		exists, err := existsCache.ContentExists(decompressedHash, struct{ RoutingKey string }{RoutingKey: decompressedHash})
		if err != nil {
			log.Warn().Err(err).Str("hash", decompressedHash).Msg("failed to check content cache before layer store")
		} else if exists {
			log.Info().Str("hash", decompressedHash).Msg("decompressed layer already present in content cache")
			return nil
		}
	}

	if localStore, ok := s.contentCache.(ContentCacheStoreLocalPath); ok && localStore != nil {
		actualHash, err := localStore.StoreContentFromLocalPath(diskPath, decompressedHash, struct{ RoutingKey string }{RoutingKey: decompressedHash})
		if err != nil {
			log.Error().Err(err).Str("hash", decompressedHash).Msg("content cache local-path store failed")
			return err
		}
		if actualHash != "" && actualHash != decompressedHash {
			return fmt.Errorf("content cache local-path hash mismatch: expected %s, got %s", decompressedHash, actualHash)
		}
		return nil
	}

	chunks := make(chan []byte, 1)
	done := make(chan struct{})
	streamErr := make(chan error, 1)
	go func() {
		defer close(chunks)
		streamErr <- streamFileInChunksUntil(diskPath, chunks, done)
	}()

	_, err := s.contentCache.StoreContent(chunks, decompressedHash, struct{ RoutingKey string }{RoutingKey: decompressedHash})
	close(done)
	if err != nil {
		log.Error().Err(err).Str("hash", decompressedHash).Msg("content cache store failed")
		if stream := <-streamErr; stream != nil {
			return fmt.Errorf("content cache store failed: %w; stream failed: %v", err, stream)
		}
		return err
	}
	if err := <-streamErr; err != nil {
		log.Error().Err(err).Str("hash", decompressedHash).Msg("failed to stream file")
		return err
	}
	return nil
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
