package clip

import (
	"archive/tar"
	// klauspost/compress gunzip is substantially faster than stdlib and is the
	// dominant cost when indexing layers (decompress + hash of every byte)
	"github.com/klauspost/compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
	"github.com/tidwall/btree"
	"golang.org/x/sync/errgroup"
)

// OCIIndexProgress represents a progress update during OCI image indexing
type OCIIndexProgress struct {
	LayerIndex   int    // Current layer being processed (1-based)
	TotalLayers  int    // Total number of layers
	LayerDigest  string // Digest of current layer
	Stage        string // "starting" or "completed"
	FilesIndexed int    // Number of files indexed so far (only for "completed")
	Message      string // Human-readable message
}

// IndexOCIImageOptions configures the OCI indexer
type IndexOCIImageOptions struct {
	ImageRef         string                            // Source image to index (can be local)
	StorageImageRef  string                            // Optional: image reference to store in metadata (defaults to ImageRef)
	CheckpointMiB    int64                             // Checkpoint every N MiB (default 2)
	CredProvider     common.RegistryCredentialProvider // optional credential provider for registry authentication
	ProgressChan     chan<- OCIIndexProgress           // optional channel for progress updates
	Platform         *v1.Platform                      // Target platform (defaults to linux/runtime.GOARCH)
	ContentCache     storage.ContentCache              // optional remote cache for fully decompressed layers
	ContentCacheDir  string                            // optional temp directory for cache upload spooling
	LayerIndexCache  storage.LayerIndexCache           // optional cache of per-layer index artifacts (skips pull+index on hit)
	IndexConcurrency int                               // max layers indexed concurrently (default 4)
}

const defaultIndexConcurrency = 4

const indexedLayerContentCacheChunkSize = 4 * 1024 * 1024

type indexedLayerContentCacheSpool struct {
	file *os.File
	path string
	err  error
}

func newIndexedLayerContentCacheSpool(dir, layerDigest string) *indexedLayerContentCacheSpool {
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Warn().Err(err).Str("dir", dir).Msg("failed to create layer content cache temp dir")
		}
	}

	file, err := os.CreateTemp(dir, "clip-index-layer-*.tar")
	if err != nil {
		log.Warn().Err(err).Str("layer_digest", layerDigest).Msg("failed to create layer content cache temp file")
		return nil
	}

	return &indexedLayerContentCacheSpool{file: file, path: file.Name()}
}

func (s *indexedLayerContentCacheSpool) Write(p []byte) (int, error) {
	if s == nil || s.file == nil {
		return len(p), nil
	}

	n, err := s.file.Write(p)
	if err != nil || n != len(p) {
		if err == nil {
			err = io.ErrShortWrite
		}
		s.err = err
		s.closeAndRemove()
		return len(p), nil
	}

	return len(p), nil
}

func (s *indexedLayerContentCacheSpool) close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *indexedLayerContentCacheSpool) closeAndRemove() {
	if s == nil {
		return
	}
	_ = s.close()
	if s.path != "" {
		_ = os.Remove(s.path)
	}
}

// countingReader tracks bytes read from an io.Reader
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	k, err := cr.r.Read(p)
	cr.n += int64(k)
	return k, err
}

// IndexOCIImage creates a metadata-only index from an OCI image
func (ca *ClipArchiver) IndexOCIImage(ctx context.Context, opts IndexOCIImageOptions) (
	index *btree.BTree,
	layerDigests []string,
	gzipIdx map[string]*common.GzipIndex,
	decompressedHashes map[string]string,
	registryURL string,
	repository string,
	reference string,
	imageMetadata *common.ImageMetadata,
	err error,
) {
	if opts.CheckpointMiB == 0 {
		opts.CheckpointMiB = 2 // default
	}

	// Parse image reference for fetching
	ref, err := name.ParseReference(opts.ImageRef)
	if err != nil {
		return nil, nil, nil, nil, "", "", "", nil, fmt.Errorf("failed to parse image reference: %w", err)
	}

	// Determine which image reference to store in metadata
	// If StorageImageRef is provided, use it; otherwise use ImageRef
	storageRef := opts.ImageRef
	if opts.StorageImageRef != "" {
		storageRef = opts.StorageImageRef
	}

	// Parse storage reference for metadata
	storageRefParsed, err := name.ParseReference(storageRef)
	if err != nil {
		return nil, nil, nil, nil, "", "", "", nil, fmt.Errorf("failed to parse storage image reference: %w", err)
	}

	// Extract registry and repository info from storage reference
	registryURL = storageRefParsed.Context().RegistryStr()
	repository = storageRefParsed.Context().RepositoryStr()
	reference = storageRefParsed.Identifier()

	// Log the indexing strategy
	if storageRef != opts.ImageRef {
		log.Debug().Msgf("Indexing from local: %s, will store reference to: %s", opts.ImageRef, storageRef)
	}

	// Determine which credential provider to use
	credProvider := opts.CredProvider
	if credProvider == nil {
		credProvider = common.DefaultProvider()
	}

	// Build remote options with authentication
	remoteOpts := []remote.Option{remote.WithContext(ctx)}

	// IMPORTANT: Get credentials for the SOURCE registry (where we're fetching from),
	// not the storage reference (which is just stored in metadata)
	fetchRegistryURL := ref.Context().RegistryStr()
	fetchRepository := ref.Context().RepositoryStr()

	// Try to get credentials from provider
	authConfig, err := credProvider.GetCredentials(ctx, fetchRegistryURL, fetchRepository)
	if err != nil && err != common.ErrNoCredentials {
		log.Warn().
			Err(err).
			Str("registry", fetchRegistryURL).
			Str("provider", credProvider.Name()).
			Msg("Failed to get credentials from provider, falling back to keychain")
	}

	if authConfig != nil {
		// Use provided credentials
		log.Debug().
			Str("registry", fetchRegistryURL).
			Str("provider", credProvider.Name()).
			Msg("Using credentials from provider")
		// Convert AuthConfig to proper authenticator (handles all auth types: username/password, tokens, etc.)
		auth := authn.FromConfig(*authConfig)
		remoteOpts = append(remoteOpts, remote.WithAuth(auth))
	} else {
		// Fall back to default keychain for anonymous or keychain-based auth
		log.Debug().
			Str("registry", fetchRegistryURL).
			Msg("No credentials from provider, using default keychain")
		remoteOpts = append(remoteOpts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}

	// Add platform option (default to host architecture)
	platform := opts.Platform
	if platform == nil {
		platform = &v1.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		}
	}
	remoteOpts = append(remoteOpts, remote.WithPlatform(*platform))
	log.Debug().
		Str("os", platform.OS).
		Str("arch", platform.Architecture).
		Msg("Using platform for image fetch")

	// Fetch image
	img, err := remote.Image(ref, remoteOpts...)
	if err != nil {
		return nil, nil, nil, nil, "", "", "", nil, fmt.Errorf("failed to fetch image: %w", err)
	}
	imageDigest, err := img.Digest()
	if err != nil {
		return nil, nil, nil, nil, "", "", "", nil, fmt.Errorf("failed to get image digest: %w", err)
	}
	reference = imageDigest.String()

	// Extract image metadata
	imageMetadata, err = ca.extractImageMetadata(img, opts.ImageRef)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to extract image metadata, continuing without it")
		imageMetadata = nil
	}

	// Get image layers
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, nil, nil, "", "", "", nil, fmt.Errorf("failed to get layers: %w", err)
	}

	// Initialize index and maps
	index = ca.newIndex()
	layerDigests = make([]string, 0, len(layers))
	gzipIdx = make(map[string]*common.GzipIndex)
	decompressedHashes = make(map[string]string)

	// Create the root node with deterministic timestamps derived from the
	// image config's created time, so indexing the same image digest always
	// produces byte-identical metadata.
	rootTime := time.Unix(0, 0)
	if imageMetadata != nil && !imageMetadata.Created.IsZero() {
		rootTime = imageMetadata.Created
	}
	root := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Ino:       1,
			Size:      0,
			Blocks:    0,
			Atime:     uint64(rootTime.Unix()),
			Atimensec: uint32(rootTime.Nanosecond()),
			Mtime:     uint64(rootTime.Unix()),
			Mtimensec: uint32(rootTime.Nanosecond()),
			Ctime:     uint64(rootTime.Unix()),
			Ctimensec: uint32(rootTime.Nanosecond()),
			Mode:      uint32(syscall.S_IFDIR | 0755),
			Nlink:     2, // Directories start with link count of 2 (. and ..)
			Owner: fuse.Owner{
				Uid: 0, // root
				Gid: 0, // root
			},
		},
	}
	index.Set(root)

	// Resolve all layer digests up front (cheap; manifest is already fetched)
	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return nil, nil, nil, nil, "", "", "", nil, fmt.Errorf("failed to get layer digest: %w", err)
		}
		layerDigests = append(layerDigests, digest.String())
	}

	log.Info().Msgf("Indexing %d layers from %s", len(layers), opts.ImageRef)

	// Index layers concurrently. Each layer produces a self-contained,
	// deterministic artifact; artifacts are merged sequentially in layer
	// order below, which preserves overlay (whiteout/replace) semantics.
	artifacts := make([]*LayerArtifact, len(layers))
	cacheHits := make([]bool, len(layers))

	concurrency := opts.IndexConcurrency
	if concurrency <= 0 {
		concurrency = defaultIndexConcurrency
	}
	if concurrency > len(layers) {
		concurrency = len(layers)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for i := range layers {
		i := i
		layer := layers[i]
		layerDigestStr := layerDigests[i]

		g.Go(func() error {
			if opts.ProgressChan != nil {
				opts.ProgressChan <- OCIIndexProgress{
					LayerIndex:  i + 1,
					TotalLayers: len(layers),
					LayerDigest: layerDigestStr,
					Stage:       "starting",
					Message:     fmt.Sprintf("Processing layer %d/%d", i+1, len(layers)),
				}
			}

			// Fast path: reuse a cached layer index artifact and skip the
			// registry pull + decompression entirely.
			cacheKey := LayerArtifactCacheKey(layerDigestStr, opts.CheckpointMiB)
			if opts.LayerIndexCache != nil {
				data, err := opts.LayerIndexCache.GetLayerIndex(gctx, cacheKey)
				if err != nil {
					log.Warn().Err(err).Str("layer_digest", layerDigestStr).Msg("layer index cache lookup failed")
				} else if data != nil {
					artifact, err := DecodeLayerArtifact(data, layerDigestStr, opts.CheckpointMiB)
					if err != nil {
						log.Warn().Err(err).Str("layer_digest", layerDigestStr).Msg("discarding invalid cached layer index artifact")
					} else {
						artifacts[i] = artifact
						cacheHits[i] = true
						log.Debug().
							Str("layer_digest", layerDigestStr).
							Int("entries", len(artifact.Entries)).
							Msg("layer index cache hit: skipping layer pull")
						return nil
					}
				}
			}

			log.Debug().Msgf("Processing layer %d/%d: %s", i+1, len(layers), layerDigestStr)

			artifact, err := ca.indexLayerFromBestSource(gctx, layer, layerDigestStr, opts)
			if err != nil {
				return fmt.Errorf("failed to index layer %s: %w", layerDigestStr, err)
			}
			artifacts[i] = artifact

			if opts.LayerIndexCache != nil {
				data, err := EncodeLayerArtifact(artifact)
				if err != nil {
					log.Warn().Err(err).Str("layer_digest", layerDigestStr).Msg("failed to encode layer index artifact")
				} else if err := opts.LayerIndexCache.PutLayerIndex(gctx, cacheKey, data); err != nil {
					log.Warn().Err(err).Str("layer_digest", layerDigestStr).Msg("failed to store layer index artifact in cache")
				}
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, nil, "", "", "", nil, err
	}

	// Merge artifacts strictly in layer order (bottom to top). This is the
	// only step that mutates the shared index, so the result is identical to
	// sequential indexing regardless of the concurrency above.
	for i, artifact := range artifacts {
		layerDigestStr := layerDigests[i]
		ca.applyLayerArtifact(index, artifact)

		gzipIdx[layerDigestStr] = &common.GzipIndex{
			LayerDigest: layerDigestStr,
			Checkpoints: artifact.Checkpoints,
		}
		decompressedHashes[layerDigestStr] = artifact.DecompressedHash

		if opts.ProgressChan != nil {
			opts.ProgressChan <- OCIIndexProgress{
				LayerIndex:   i + 1,
				TotalLayers:  len(layers),
				LayerDigest:  layerDigestStr,
				Stage:        "completed",
				FilesIndexed: index.Len(),
				Message:      fmt.Sprintf("Completed layer %d/%d (%d files total)", i+1, len(layers), index.Len()),
			}
		}
	}

	cachedLayers := 0
	for _, hit := range cacheHits {
		if hit {
			cachedLayers++
		}
	}
	log.Info().
		Int("layers", len(layers)).
		Int("layer_index_cache_hits", cachedLayers).
		Int("files", index.Len()).
		Msg("Successfully indexed image")

	return index, layerDigests, gzipIdx, decompressedHashes, registryURL, repository, reference, imageMetadata, nil
}

// indexLayerToArtifact processes a single layer using streaming I/O with zero memory overhead.
//
// Performance characteristics:
//   - Zero-copy streaming: TeeReader hashes data as it flows to tar.Reader
//   - Constant memory: O(checkpoint_size) ~2MB, independent of layer size
//   - Single pass: Reads compressed stream exactly once
//
// The result is a self-contained, deterministic LayerArtifact (ordered entry
// operations + gzip checkpoints + decompressed stream hash) that can be cached
// keyed by the layer digest and replayed against an index with
// applyLayerArtifact.
func (ca *ClipArchiver) indexLayerToArtifact(
	ctx context.Context,
	compressedRC io.ReadCloser,
	layerDigest string,
	opts IndexOCIImageOptions,
) (*LayerArtifact, error) {
	compressedCounter := &countingReader{r: compressedRC}

	gzr, err := gzip.NewReader(compressedCounter)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	var cacheSpool *indexedLayerContentCacheSpool
	if opts.ContentCache != nil {
		cacheSpool = newIndexedLayerContentCacheSpool(opts.ContentCacheDir, layerDigest)
		defer cacheSpool.closeAndRemove()
	}

	// Streaming hash computation via TeeReader.
	// When a content cache is configured, the same decompressed byte stream is
	// also spooled once so the runtime does not decompress this layer again.
	hasher := sha256.New()
	hashWriter := io.Writer(hasher)
	if cacheSpool != nil {
		hashWriter = io.MultiWriter(hasher, cacheSpool)
	}
	hashingReader := io.TeeReader(gzr, hashWriter)
	uncompressedCounter := &countingReader{r: hashingReader}
	tr := tar.NewReader(uncompressedCounter)

	// Pre-allocate checkpoint slice (estimate: 1 per 2MB, typical layer is 50-200MB)
	checkpoints := make([]common.GzipCheckpoint, 0, 64)
	checkpointInterval := opts.CheckpointMiB * 1024 * 1024
	lastCheckpoint := int64(0)

	entries := make([]LayerEntry, 0, 256)

	// Process tar entries
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar header: %w", err)
		}

		// Record checkpoint periodically (before processing file data)
		if uncompressedCounter.n-lastCheckpoint >= checkpointInterval {
			ca.addCheckpoint(&checkpoints, compressedCounter.n, uncompressedCounter.n, &lastCheckpoint)
		}

		// Normalize path (remove ./ prefix, ensure leading slash)
		cleanPath := hdr.Name
		if strings.HasPrefix(cleanPath, "./") {
			cleanPath = cleanPath[1:] // Keep leading slash: "./foo" -> "/foo"
		} else if !strings.HasPrefix(cleanPath, "/") {
			cleanPath = "/" + cleanPath // Ensure leading slash
		}
		cleanPath = path.Clean(cleanPath)

		// Handle OCI whiteouts (fast path: check prefix before full processing)
		if entry, isWhiteout := whiteoutEntry(cleanPath); isWhiteout {
			entries = append(entries, entry)
			continue
		}

		// Process based on type (most common first for branch prediction)
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			entry, err := ca.fileEntry(tr, hdr, cleanPath, layerDigest, compressedCounter, uncompressedCounter, &checkpoints, &lastCheckpoint)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		case tar.TypeDir:
			entries = append(entries, ca.directoryEntry(hdr, cleanPath, layerDigest))
		case tar.TypeSymlink:
			entries = append(entries, ca.symlinkEntry(hdr, cleanPath, layerDigest))
		case tar.TypeLink:
			targetPath := path.Clean("/" + strings.TrimPrefix(hdr.Linkname, "./"))
			entries = append(entries, LayerEntry{
				Kind:   LayerEntryHardLink,
				Path:   cleanPath,
				Target: targetPath,
			})
		}
	}

	// Add final checkpoint if needed
	if uncompressedCounter.n > lastCheckpoint {
		ca.addCheckpoint(&checkpoints, compressedCounter.n, uncompressedCounter.n, &lastCheckpoint)
	}

	// Consume trailing TAR padding/EOF blocks that tar.Reader doesn't expose.
	// These bytes ARE present in decompressed stream and MUST be hashed to match disk cache.
	_, err = io.Copy(io.Discard, uncompressedCounter)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to consume trailing tar bytes: %w", err)
	}

	// Finalize hash (includes all bytes: file contents + tar headers + padding)
	decompressedHash := hex.EncodeToString(hasher.Sum(nil))

	if opts.ContentCache != nil && cacheSpool != nil && cacheSpool.err == nil && cacheSpool.path != "" {
		if err := cacheSpool.close(); err != nil {
			return nil, fmt.Errorf("failed to close layer content cache temp file: %w", err)
		}

		if err := ca.storeIndexedLayerInContentCache(ctx, opts.ContentCache, cacheSpool.path, decompressedHash, layerDigest); err != nil {
			log.Warn().
				Err(err).
				Str("layer_digest", layerDigest).
				Str("decompressed_hash", decompressedHash).
				Msg("failed to store indexed layer in content cache")
		}
	} else if cacheSpool != nil && cacheSpool.err != nil {
		log.Warn().
			Err(cacheSpool.err).
			Str("layer_digest", layerDigest).
			Msg("skipping indexed layer content cache store after spool write failure")
	}

	return &LayerArtifact{
		Version:          LayerArtifactVersion,
		LayerDigest:      layerDigest,
		CheckpointMiB:    opts.CheckpointMiB,
		Entries:          entries,
		Checkpoints:      checkpoints,
		DecompressedHash: decompressedHash,
		UncompressedSize: uncompressedCounter.n,
	}, nil
}

func (ca *ClipArchiver) storeIndexedLayerInContentCache(ctx context.Context, contentCache storage.ContentCache, filePath, decompressedHash, layerDigest string) error {
	return ca.storeLayerBlobInContentCache(ctx, contentCache, filePath, decompressedHash, layerDigest, "indexed layer")
}

// storeLayerBlobInContentCache stores a file's bytes in the content cache
// under the given content hash, skipping the store if a size-aware existence
// check reports the blob is already complete.
func (ca *ClipArchiver) storeLayerBlobInContentCache(ctx context.Context, contentCache storage.ContentCache, filePath, contentHash, layerDigest, kind string) error {
	if contentCache == nil {
		return nil
	}

	// Skip the store when the blob is already complete in the cache,
	// preferring the size-aware check (a positive answer guarantees the
	// cached blob is complete).
	exists := false
	if info, err := os.Stat(filePath); err == nil {
		exists = contentCacheExistsWithSize(contentCache, contentHash, info.Size())
	}
	if !exists {
		if existsCache, ok := contentCache.(storage.ContentCacheExists); ok {
			if found, err := existsCache.ContentExists(contentHash, struct{ RoutingKey string }{RoutingKey: contentHash}); err == nil && found {
				exists = true
			}
		}
	}
	if exists {
		log.Debug().Str("layer_digest", layerDigest).Msgf("%s already present in content cache", kind)
		return nil
	}

	if localStore, ok := contentCache.(storage.ContentCacheStoreLocalPath); ok && localStore != nil {
		actualHash, err := localStore.StoreContentFromLocalPath(filePath, contentHash, struct{ RoutingKey string }{RoutingKey: contentHash})
		if err != nil {
			return err
		}
		if actualHash != "" && actualHash != contentHash {
			return fmt.Errorf("%s content cache hash mismatch: expected %s, got %s", kind, contentHash, actualHash)
		}
		log.Debug().Str("layer_digest", layerDigest).Msgf("stored %s in content cache", kind)
		return nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open %s temp file: %w", kind, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s temp file: %w", kind, err)
	}

	storeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chunks := make(chan []byte, 2)
	readErrCh := make(chan error, 1)
	go func() {
		defer close(chunks)
		buf := make([]byte, indexedLayerContentCacheChunkSize)
		for {
			n, readErr := file.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case chunks <- chunk:
				case <-storeCtx.Done():
					readErrCh <- storeCtx.Err()
					return
				}
			}
			if readErr == io.EOF {
				readErrCh <- nil
				return
			}
			if readErr != nil {
				readErrCh <- readErr
				return
			}
		}
	}()

	started := time.Now()
	actualHash, storeErr := contentCache.StoreContent(chunks, contentHash, struct{ RoutingKey string }{RoutingKey: contentHash})
	cancel()
	readErr := <-readErrCh
	if storeErr != nil {
		return storeErr
	}
	if readErr != nil {
		return readErr
	}
	if actualHash != "" && actualHash != contentHash {
		return fmt.Errorf("%s content cache hash mismatch: expected %s, got %s", kind, contentHash, actualHash)
	}

	log.Debug().
		Str("layer_digest", layerDigest).
		Int64("bytes", info.Size()).
		Dur("duration", time.Since(started)).
		Msgf("stored %s in content cache", kind)

	return nil
}

// whiteoutEntry detects OCI whiteout files and returns the corresponding
// layer entry operation.
func whiteoutEntry(fullPath string) (LayerEntry, bool) {
	dir := path.Dir(fullPath)
	base := path.Base(fullPath)

	// Opaque whiteout: .wh..wh..opq
	if base == ".wh..wh..opq" {
		log.Debug().Msgf("  Opaque whiteout: %s", dir)
		return LayerEntry{Kind: LayerEntryOpaqueWhiteout, Path: dir}, true
	}

	// Regular whiteout: .wh.<name>
	if strings.HasPrefix(base, ".wh.") {
		victim := path.Join(dir, strings.TrimPrefix(base, ".wh."))
		log.Debug().Msgf("  Whiteout: %s", victim)
		return LayerEntry{Kind: LayerEntryWhiteout, Path: victim}, true
	}

	return LayerEntry{}, false
}

// deleteNode removes a node and all its children from the index
func (ca *ClipArchiver) deleteNode(index *btree.BTree, nodePath string) {
	// Remove the node itself
	index.Delete(&common.ClipNode{Path: nodePath})

	// Remove all children (for directories)
	ca.deleteRange(index, nodePath+"/")
}

// deleteRange removes all nodes with paths starting with prefix
func (ca *ClipArchiver) deleteRange(index *btree.BTree, prefix string) {
	var toDelete []*common.ClipNode

	pivot := &common.ClipNode{Path: prefix}
	index.Ascend(pivot, func(a interface{}) bool {
		node := a.(*common.ClipNode)
		if strings.HasPrefix(node.Path, prefix) {
			toDelete = append(toDelete, node)
			return true
		}
		return false // stop iteration once we're past the prefix
	})

	for _, node := range toDelete {
		index.Delete(node)
	}
}

// isRuntimeDirectory checks if a path is a special runtime directory
// that should be mounted by the container runtime, not included in the image
func (ca *ClipArchiver) isRuntimeDirectory(path string) bool {
	runtimeDirs := []string{
		"/proc",
		"/sys",
		"/dev",
	}

	for _, dir := range runtimeDirs {
		if path == dir {
			return true
		}
	}

	return false
}

// tarModeToFuse converts tar mode to FUSE mode
func (ca *ClipArchiver) tarModeToFuse(tarMode int64, typeflag byte) uint32 {
	mode := uint32(tarMode & 0777) // permission bits

	switch typeflag {
	case tar.TypeDir:
		mode |= syscall.S_IFDIR
	case tar.TypeSymlink:
		mode |= syscall.S_IFLNK
	case tar.TypeReg, tar.TypeRegA:
		mode |= syscall.S_IFREG
	default:
		mode |= syscall.S_IFREG
	}

	return mode
}

// generateInode creates a stable inode number from digest and path
func (ca *ClipArchiver) generateInode(digest string, path string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(digest))
	h.Write([]byte(path))
	inode := h.Sum64()

	// Ensure inode is never 0 (reserved for errors) or 1 (reserved for root)
	if inode <= 1 {
		inode = 2
	}

	return inode
}

// CreateFromOCI creates a metadata-only .clip file from an OCI image
func (ca *ClipArchiver) CreateFromOCI(ctx context.Context, opts IndexOCIImageOptions, clipOut string) error {
	// Index the OCI image
	index, layers, gzipIdx, decompressedHashes, registryURL, repository, reference, imageMetadata, err := ca.IndexOCIImage(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to index OCI image: %w", err)
	}

	// Create OCIStorageInfo
	storageInfo := &common.OCIStorageInfo{
		RegistryURL:             registryURL,
		Repository:              repository,
		Reference:               reference,
		Layers:                  layers,
		GzipIdxByLayer:          gzipIdx,
		ZstdIdxByLayer:          nil, // P1 feature
		DecompressedHashByLayer: decompressedHashes,
		ImageMetadata:           imageMetadata,
	}

	// Create metadata
	metadata := &common.ClipArchiveMetadata{
		Index:       index,
		StorageInfo: storageInfo,
	}

	// Write metadata-only clip file
	err = ca.CreateRemoteArchive(storageInfo, metadata, clipOut)
	if err != nil {
		return fmt.Errorf("failed to create remote archive: %w", err)
	}

	totalCheckpoints := 0
	for _, idx := range gzipIdx {
		totalCheckpoints += len(idx.Checkpoints)
	}
	log.Debug().
		Str("path", clipOut).
		Int("files", index.Len()).
		Int("layers", len(layers)).
		Int("gzip_checkpoints", totalCheckpoints).
		Msg("created metadata-only clip file")

	return nil
}

// addCheckpoint adds a gzip checkpoint and updates lastCheckpoint.
// Inlined for performance (called frequently during indexing).
func (ca *ClipArchiver) addCheckpoint(checkpoints *[]common.GzipCheckpoint, cOff, uOff int64, lastCheckpoint *int64) {
	*checkpoints = append(*checkpoints, common.GzipCheckpoint{COff: cOff, UOff: uOff})
	*lastCheckpoint = uOff
}

// fileEntry processes a regular file entry from tar.
// Uses io.CopyN for efficient content skipping (streaming, no allocation).
func (ca *ClipArchiver) fileEntry(
	tr *tar.Reader,
	hdr *tar.Header,
	cleanPath string,
	layerDigest string,
	compressedCounter *countingReader,
	uncompressedCounter *countingReader,
	checkpoints *[]common.GzipCheckpoint,
	lastCheckpoint *int64,
) (LayerEntry, error) {
	dataStart := uncompressedCounter.n

	// Content-defined checkpoint for large files (>512KB)
	// Enables fast seeking to file start without full layer decompression
	const largeFileThreshold = 512 * 1024
	const minCheckpointGap = 512 * 1024

	if hdr.Size > largeFileThreshold && (uncompressedCounter.n-*lastCheckpoint) >= minCheckpointGap {
		ca.addCheckpoint(checkpoints, compressedCounter.n, uncompressedCounter.n, lastCheckpoint)
	}

	// Skip file content (streaming, zero-copy via io.Discard)
	if hdr.Size > 0 {
		n, err := io.CopyN(io.Discard, tr, hdr.Size)
		if err != nil && err != io.EOF {
			return LayerEntry{}, fmt.Errorf("failed to skip file content: %w", err)
		}
		if n != hdr.Size {
			return LayerEntry{}, fmt.Errorf("incomplete file read: want %d, got %d", hdr.Size, n)
		}
	}

	node := &common.ClipNode{
		Path:     cleanPath,
		NodeType: common.FileNode,
		Attr: fuse.Attr{
			Ino:       ca.generateInode(layerDigest, cleanPath),
			Size:      uint64(hdr.Size),
			Blocks:    (uint64(hdr.Size) + 511) / 512,
			Atime:     uint64(hdr.AccessTime.Unix()),
			Atimensec: uint32(hdr.AccessTime.Nanosecond()),
			Mtime:     uint64(hdr.ModTime.Unix()),
			Mtimensec: uint32(hdr.ModTime.Nanosecond()),
			Ctime:     uint64(hdr.ChangeTime.Unix()),
			Ctimensec: uint32(hdr.ChangeTime.Nanosecond()),
			Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeReg),
			Nlink:     1,
			Owner: fuse.Owner{
				Uid: uint32(hdr.Uid),
				Gid: uint32(hdr.Gid),
			},
		},
		Remote: &common.RemoteRef{
			LayerDigest: layerDigest,
			UOffset:     dataStart,
			ULength:     hdr.Size,
		},
	}

	log.Debug().Str("path", cleanPath).Int64("size", hdr.Size).Int64("uoff", dataStart).Msg("File")
	return LayerEntry{Kind: LayerEntryNode, Node: node}, nil
}

// symlinkEntry processes a symlink entry from tar
func (ca *ClipArchiver) symlinkEntry(hdr *tar.Header, cleanPath, layerDigest string) LayerEntry {
	target := hdr.Linkname
	if target == "" {
		log.Warn().Msgf("Empty symlink target for %s", cleanPath)
	}

	node := &common.ClipNode{
		Path:     cleanPath,
		NodeType: common.SymLinkNode,
		Target:   target,
		Attr: fuse.Attr{
			Ino:       ca.generateInode(layerDigest, cleanPath),
			Size:      uint64(len(target)),
			Blocks:    0,
			Atime:     uint64(hdr.AccessTime.Unix()),
			Atimensec: uint32(hdr.AccessTime.Nanosecond()),
			Mtime:     uint64(hdr.ModTime.Unix()),
			Mtimensec: uint32(hdr.ModTime.Nanosecond()),
			Ctime:     uint64(hdr.ChangeTime.Unix()),
			Ctimensec: uint32(hdr.ChangeTime.Nanosecond()),
			Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeSymlink),
			Nlink:     1,
			Owner: fuse.Owner{
				Uid: uint32(hdr.Uid),
				Gid: uint32(hdr.Gid),
			},
		},
	}

	log.Debug().Str("path", cleanPath).Str("target", target).Msg("Symlink")
	return LayerEntry{Kind: LayerEntryNode, Node: node}
}

// directoryEntry processes a directory entry from tar
func (ca *ClipArchiver) directoryEntry(hdr *tar.Header, cleanPath, layerDigest string) LayerEntry {
	node := &common.ClipNode{
		Path:     cleanPath,
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Ino:       ca.generateInode(layerDigest, cleanPath),
			Size:      0,
			Blocks:    0,
			Atime:     uint64(hdr.AccessTime.Unix()),
			Atimensec: uint32(hdr.AccessTime.Nanosecond()),
			Mtime:     uint64(hdr.ModTime.Unix()),
			Mtimensec: uint32(hdr.ModTime.Nanosecond()),
			Ctime:     uint64(hdr.ChangeTime.Unix()),
			Ctimensec: uint32(hdr.ChangeTime.Nanosecond()),
			Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeDir),
			Nlink:     2,
			Owner: fuse.Owner{
				Uid: uint32(hdr.Uid),
				Gid: uint32(hdr.Gid),
			},
		},
	}

	log.Debug().Str("path", cleanPath).Int64("mode", hdr.Mode).Int64("mtime", hdr.ModTime.Unix()).Msg("Dir")
	return LayerEntry{Kind: LayerEntryNode, Node: node}
}

// extractImageMetadata extracts comprehensive metadata from an OCI image
func (ca *ClipArchiver) extractImageMetadata(imgInterface interface{}, imageRef string) (*common.ImageMetadata, error) {
	// Type assert to v1.Image from go-containerregistry
	img, ok := imgInterface.(v1.Image)
	if !ok {
		return nil, fmt.Errorf("image does not implement v1.Image interface, got type %T", imgInterface)
	}

	// Get config file
	configFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("failed to get config file: %w", err)
	}

	// Get digest
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("failed to get digest: %w", err)
	}

	// Get manifest for layer information
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest: %w", err)
	}

	// Extract layer metadata from manifest
	layersData := make([]common.LayerMetadata, 0, len(manifest.Layers))
	layers := make([]string, 0, len(manifest.Layers))

	for _, layer := range manifest.Layers {
		layersData = append(layersData, common.LayerMetadata{
			MIMEType:    string(layer.MediaType),
			Digest:      layer.Digest.String(),
			Size:        layer.Size,
			Annotations: layer.Annotations,
		})
		layers = append(layers, layer.Digest.String())
	}

	// Extract created time
	createdTime := configFile.Created.Time

	// Initialize empty maps/slices if nil to ensure compatibility
	labels := configFile.Config.Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	env := configFile.Config.Env
	if env == nil {
		env = make([]string, 0)
	}

	exposedPorts := configFile.Config.ExposedPorts
	if exposedPorts == nil {
		exposedPorts = make(map[string]struct{})
	}

	volumes := configFile.Config.Volumes
	if volumes == nil {
		volumes = make(map[string]struct{})
	}

	// Build metadata structure
	metadata := &common.ImageMetadata{
		Name:          imageRef,
		Digest:        digest.String(),
		Created:       createdTime,
		DockerVersion: configFile.DockerVersion,
		Architecture:  configFile.Architecture,
		Os:            configFile.OS,
		Variant:       configFile.Variant,
		Author:        configFile.Author,
		Labels:        labels,
		Env:           env,
		Cmd:           configFile.Config.Cmd,
		Entrypoint:    configFile.Config.Entrypoint,
		User:          configFile.Config.User,
		WorkingDir:    configFile.Config.WorkingDir,
		ExposedPorts:  exposedPorts,
		Volumes:       volumes,
		StopSignal:    configFile.Config.StopSignal,
		Layers:        layers,
		LayersData:    layersData,
	}

	log.Debug().
		Str("architecture", metadata.Architecture).
		Str("os", metadata.Os).
		Time("created", metadata.Created).
		Int("layers", len(metadata.Layers)).
		Msg("Extracted image metadata")

	return metadata, nil
}
