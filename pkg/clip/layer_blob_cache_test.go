package clip

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBlobContentCache is an in-memory content-addressed cache implementing
// the optional extensions clip uses during indexing.
type fakeBlobContentCache struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newFakeBlobContentCache() *fakeBlobContentCache {
	return &fakeBlobContentCache{blobs: map[string][]byte{}}
}

func (c *fakeBlobContentCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.blobs[hash]
	if !ok || offset+length > int64(len(data)) {
		return nil, fmt.Errorf("content not found: %s", hash)
	}
	return data[offset : offset+length], nil
}

func (c *fakeBlobContentCache) ReadContentInto(hash string, offset int64, dest []byte, opts struct{ RoutingKey string }) (int64, error) {
	data, err := c.GetContent(hash, offset, int64(len(dest)), opts)
	if err != nil {
		return 0, err
	}
	return int64(copy(dest, data)), nil
}

func (c *fakeBlobContentCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	var buf bytes.Buffer
	for chunk := range chunks {
		buf.Write(chunk)
	}
	return c.put(buf.Bytes()), nil
}

func (c *fakeBlobContentCache) StoreContentFromLocalPath(path string, hash string, opts struct{ RoutingKey string }) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return c.put(data), nil
}

func (c *fakeBlobContentCache) ContentExistsWithSize(hash string, size int64, opts struct{ RoutingKey string }) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.blobs[hash]
	return ok && int64(len(data)) == size, nil
}

func (c *fakeBlobContentCache) put(data []byte) string {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[hash] = append([]byte(nil), data...)
	return hash
}

func (c *fakeBlobContentCache) has(hash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.blobs[hash]
	return ok
}

// TestCompressedLayerContentCacheReadThrough verifies that indexing warms
// compressed layer blobs into the content cache (keyed by layer digest) and
// that subsequent indexing reads layers from the cache instead of the
// registry, producing identical output.
func TestCompressedLayerContentCacheReadThrough(t *testing.T) {
	// Build a 2-layer synthetic image
	img := empty.Image
	for layerIdx := 0; layerIdx < 2; layerIdx++ {
		entries := []tarEntry{
			{name: fmt.Sprintf("dir%d/", layerIdx), typeflag: tar.TypeDir},
			{name: fmt.Sprintf("dir%d/file.txt", layerIdx), typeflag: tar.TypeReg, content: strings.Repeat("x", 4096)},
		}
		blob := buildLayer(t, entries)
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(blob)), nil
		})
		require.NoError(t, err)
		img, err = mutate.AppendLayers(img, layer)
		require.NoError(t, err)
	}

	layers, err := img.Layers()
	require.NoError(t, err)
	layerDigests := make([]string, 0, len(layers))
	for _, l := range layers {
		d, err := l.Digest()
		require.NoError(t, err)
		layerDigests = append(layerDigests, d.String())
	}

	// Serve via in-memory registry, counting layer blob fetches
	var mu sync.Mutex
	layerBlobGets := 0
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			for _, d := range layerDigests {
				if strings.Contains(r.URL.Path, d) {
					mu.Lock()
					layerBlobGets++
					mu.Unlock()
				}
			}
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	imageRef := strings.TrimPrefix(srv.URL, "http://") + "/test/blobcache:latest"
	ref, err := name.ParseReference(imageRef)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))

	archiver := NewClipArchiver()
	cache := newFakeBlobContentCache()
	opts := IndexOCIImageOptions{
		ImageRef:        imageRef,
		CheckpointMiB:   2,
		ContentCache:    cache,
		ContentCacheDir: t.TempDir(),
		Platform:        &v1.Platform{OS: "linux", Architecture: "amd64"},
	}

	// Run 1: cold — layers pulled from registry, compressed blobs warmed
	idx1, _, _, hashes1, _, _, _, _, err := archiver.IndexOCIImage(context.Background(), opts)
	require.NoError(t, err)
	mu.Lock()
	getsAfterCold := layerBlobGets
	mu.Unlock()
	require.Greater(t, getsAfterCold, 0, "cold run must pull layers from registry")

	for _, d := range layerDigests {
		assert.True(t, cache.has(compressedLayerCacheKey(d)), "compressed blob %s must be warmed into content cache", d)
	}

	// Run 2: layers must come from the content cache, not the registry
	idx2, _, _, hashes2, _, _, _, _, err := archiver.IndexOCIImage(context.Background(), opts)
	require.NoError(t, err)
	mu.Lock()
	getsAfterWarm := layerBlobGets
	mu.Unlock()
	assert.Equal(t, getsAfterCold, getsAfterWarm, "warm run must not fetch layer blobs from registry")

	// Outputs identical
	bytes1, err := archiver.EncodeIndex(idx1)
	require.NoError(t, err)
	bytes2, err := archiver.EncodeIndex(idx2)
	require.NoError(t, err)
	assert.Equal(t, bytes1, bytes2, "index must be identical regardless of layer source")
	assert.Equal(t, hashes1, hashes2)
}
