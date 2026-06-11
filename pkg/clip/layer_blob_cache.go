package clip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/beam-cloud/clip/pkg/storage"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	log "github.com/rs/zerolog/log"
)

const contentCacheBlobReadChunkSize = 4 * 1024 * 1024

// compressedLayerCacheKey returns the content-cache key for a compressed
// layer blob. OCI layer digests are the sha256 of the compressed bytes, so
// the hex digest doubles as the content-addressed cache key.
func compressedLayerCacheKey(layerDigest string) string {
	return strings.TrimPrefix(layerDigest, "sha256:")
}

// contentCacheExistsWithSize performs a size-aware existence check when the
// cache supports it. Only a size-aware positive answer is trusted, since it
// guarantees the cached blob is complete.
func contentCacheExistsWithSize(cache storage.ContentCache, key string, size int64) bool {
	if cache == nil || size <= 0 {
		return false
	}
	sizeCache, ok := cache.(storage.ContentCacheExistsWithSize)
	if !ok {
		return false
	}
	exists, err := sizeCache.ContentExistsWithSize(key, size, struct{ RoutingKey string }{RoutingKey: key})
	if err != nil {
		log.Debug().Err(err).Str("key", key).Msg("content cache exists check failed")
		return false
	}
	return exists
}

// contentCacheBlobReader streams a content-cache blob sequentially with
// read-ahead buffering, so the gzip reader's small reads don't translate into
// per-read cache RPCs.
type contentCacheBlobReader struct {
	cache    storage.ContentCache
	readInto storage.ContentCacheReadInto
	key      string
	size     int64
	offset   int64
	buf      []byte
	bufPos   int
}

func newContentCacheBlobReader(cache storage.ContentCache, key string, size int64) *contentCacheBlobReader {
	r := &contentCacheBlobReader{cache: cache, key: key, size: size}
	if ri, ok := cache.(storage.ContentCacheReadInto); ok {
		r.readInto = ri
	}
	return r
}

func (r *contentCacheBlobReader) refill() error {
	remaining := r.size - r.offset
	if remaining <= 0 {
		return io.EOF
	}
	length := int64(contentCacheBlobReadChunkSize)
	if length > remaining {
		length = remaining
	}

	opts := struct{ RoutingKey string }{RoutingKey: r.key}
	if r.readInto != nil {
		if cap(r.buf) < int(length) {
			r.buf = make([]byte, length)
		}
		r.buf = r.buf[:length]
		n, err := r.readInto.ReadContentInto(r.key, r.offset, r.buf, opts)
		if err != nil {
			return err
		}
		if n != length {
			return fmt.Errorf("short content cache read: expected %d bytes, got %d", length, n)
		}
	} else {
		data, err := r.cache.GetContent(r.key, r.offset, length, opts)
		if err != nil {
			return err
		}
		if int64(len(data)) != length {
			return fmt.Errorf("short content cache read: expected %d bytes, got %d", length, len(data))
		}
		r.buf = data
	}

	r.offset += length
	r.bufPos = 0
	return nil
}

func (r *contentCacheBlobReader) Read(p []byte) (int, error) {
	if r.bufPos >= len(r.buf) {
		if err := r.refill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.bufPos:])
	r.bufPos += n
	return n, nil
}

func (r *contentCacheBlobReader) Close() error { return nil }

// hashingReadCloser hashes every byte read from the underlying reader so the
// stream can be verified against the layer digest after consumption.
type hashingReadCloser struct {
	rc     io.ReadCloser
	hasher hash.Hash
}

func newHashingReadCloser(rc io.ReadCloser) *hashingReadCloser {
	return &hashingReadCloser{rc: rc, hasher: sha256.New()}
}

func (h *hashingReadCloser) Read(p []byte) (int, error) {
	n, err := h.rc.Read(p)
	if n > 0 {
		h.hasher.Write(p[:n])
	}
	return n, err
}

func (h *hashingReadCloser) Close() error { return h.rc.Close() }

func (h *hashingReadCloser) sum() string {
	return hex.EncodeToString(h.hasher.Sum(nil))
}

// indexLayerFromBestSource indexes a layer using the cheapest available
// source of its compressed bytes:
//
//  1. The content cache, keyed by the layer digest (the same content-addressed
//     mechanism used for layer caching at runtime). Hit -> no registry pull.
//  2. The registry. The compressed bytes are then warmed into the content
//     cache so other workers skip the registry pull next time.
func (ca *ClipArchiver) indexLayerFromBestSource(
	ctx context.Context,
	layer v1.Layer,
	layerDigest string,
	opts IndexOCIImageOptions,
) (*LayerArtifact, error) {
	blobKey := compressedLayerCacheKey(layerDigest)

	// Source 1: compressed blob from the content cache
	if opts.ContentCache != nil {
		compressedSize, err := layer.Size()
		if err == nil && contentCacheExistsWithSize(opts.ContentCache, blobKey, compressedSize) {
			reader := newHashingReadCloser(newContentCacheBlobReader(opts.ContentCache, blobKey, compressedSize))
			artifact, err := ca.indexLayerToArtifact(ctx, reader, layerDigest, opts)
			if err == nil {
				// Consume any trailing bytes the gzip reader didn't request,
				// then verify the full blob hash against the layer digest.
				if _, drainErr := io.Copy(io.Discard, reader); drainErr == nil && reader.sum() == blobKey {
					log.Debug().
						Str("layer_digest", layerDigest).
						Int64("compressed_bytes", compressedSize).
						Msg("compressed layer cache hit: indexed without registry pull")
					return artifact, nil
				}
				log.Warn().
					Str("layer_digest", layerDigest).
					Msg("compressed layer cache content failed digest verification; falling back to registry")
			} else {
				log.Warn().
					Err(err).
					Str("layer_digest", layerDigest).
					Msg("failed to index layer from content cache; falling back to registry")
			}
		}
	}

	// Source 2: registry pull (+ warm the compressed blob into the cache)
	compressedRC, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("failed to get compressed layer: %w", err)
	}
	defer compressedRC.Close()

	var blobSpool *indexedLayerContentCacheSpool
	var source io.Reader = compressedRC
	if opts.ContentCache != nil {
		blobSpool = newIndexedLayerContentCacheSpool(opts.ContentCacheDir, layerDigest)
		if blobSpool != nil {
			defer blobSpool.closeAndRemove()
			source = io.TeeReader(compressedRC, blobSpool)
		}
	}

	artifact, err := ca.indexLayerToArtifact(ctx, io.NopCloser(source), layerDigest, opts)
	if err != nil {
		return nil, err
	}

	if blobSpool != nil && blobSpool.err == nil && blobSpool.path != "" {
		// Drain trailing compressed bytes the gzip reader didn't consume so
		// the spool contains the complete blob.
		if _, err := io.Copy(io.Discard, source); err != nil {
			log.Warn().Err(err).Str("layer_digest", layerDigest).Msg("failed to drain compressed layer stream for cache warm")
		} else if err := blobSpool.close(); err != nil {
			log.Warn().Err(err).Str("layer_digest", layerDigest).Msg("failed to finalize compressed layer spool")
		} else if err := ca.storeLayerBlobInContentCache(ctx, opts.ContentCache, blobSpool.path, blobKey, layerDigest, "compressed layer"); err != nil {
			log.Warn().Err(err).Str("layer_digest", layerDigest).Msg("failed to store compressed layer in content cache")
		}
	}

	return artifact, nil
}
