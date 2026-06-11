package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// LayerIndexCache stores serialized per-layer index artifacts keyed by a
// deterministic string (see clip.LayerArtifactCacheKey). A hit allows the
// indexer to skip pulling and decompressing that layer entirely.
//
// Implementations must treat the cache as best-effort: Get returns
// (nil, nil) on a miss, and Put failures should not fail the build.
type LayerIndexCache interface {
	GetLayerIndex(ctx context.Context, key string) ([]byte, error)
	PutLayerIndex(ctx context.Context, key string, data []byte) error
}

// ContentCacheExistsWithSize is an optional ContentCache extension that
// performs a size-aware completeness check. Unlike ContentCacheExists, a
// positive response guarantees the cached content is complete (not a stale,
// partially-published entry), so callers can safely skip re-storing it.
type ContentCacheExistsWithSize interface {
	ContentExistsWithSize(hash string, size int64, opts struct{ RoutingKey string }) (bool, error)
}

// DiskLayerIndexCache is a simple local-filesystem LayerIndexCache. It is used
// by the test harness and can serve as a worker-local fallback cache.
type DiskLayerIndexCache struct {
	rootDir string
}

func NewDiskLayerIndexCache(rootDir string) (*DiskLayerIndexCache, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("disk layer index cache root dir is required")
	}
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create layer index cache dir: %w", err)
	}
	return &DiskLayerIndexCache{rootDir: rootDir}, nil
}

func (c *DiskLayerIndexCache) entryPath(key string) string {
	return filepath.Join(c.rootDir, filepath.FromSlash(key))
}

func (c *DiskLayerIndexCache) GetLayerIndex(ctx context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(c.entryPath(key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *DiskLayerIndexCache) PutLayerIndex(ctx context.Context, key string, data []byte) error {
	path := c.entryPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	// Write via temp file + rename so concurrent readers never observe a
	// partially written artifact.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".layer-index-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
