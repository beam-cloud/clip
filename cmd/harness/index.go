package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
)

// instrumentedLayerCache wraps a LayerIndexCache and counts hits, misses, and
// puts so runs can assert on cache behavior.
type instrumentedLayerCache struct {
	inner  storage.LayerIndexCache
	hits   atomic.Int64
	misses atomic.Int64
	puts   atomic.Int64
}

func (c *instrumentedLayerCache) GetLayerIndex(ctx context.Context, key string) ([]byte, error) {
	data, err := c.inner.GetLayerIndex(ctx, key)
	if err == nil && data != nil {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return data, err
}

func (c *instrumentedLayerCache) PutLayerIndex(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	return c.inner.PutLayerIndex(ctx, key, data)
}

// indexRun captures everything needed to compare and report on one indexing
// pass over an image.
type indexRun struct {
	label      string
	duration   time.Duration
	indexBytes []byte
	metadata   *common.ClipArchiveMetadata
	cacheHits  int64
	cachePuts  int64
}

// runIndex indexes an image with clip.CreateFromOCIImage and extracts the
// encoded index region plus decoded metadata from the resulting archive.
func runIndex(ctx context.Context, label, imageRef, outputDir string, layerCache storage.LayerIndexCache, concurrency int) (*indexRun, error) {
	outputPath := filepath.Join(outputDir, label+".clip")

	started := time.Now()
	err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
		ImageRef:         imageRef,
		OutputPath:       outputPath,
		LayerIndexCache:  layerCache,
		IndexConcurrency: concurrency,
	})
	if err != nil {
		return nil, fmt.Errorf("indexing failed (%s): %w", label, err)
	}
	duration := time.Since(started)

	archiver := clip.NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to extract metadata (%s): %w", label, err)
	}

	archiveBytes, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, err
	}
	header := metadata.Header
	if header.IndexPos+header.IndexLength > int64(len(archiveBytes)) {
		return nil, fmt.Errorf("invalid index region in %s", outputPath)
	}

	run := &indexRun{
		label:      label,
		duration:   duration,
		indexBytes: archiveBytes[header.IndexPos : header.IndexPos+header.IndexLength],
		metadata:   metadata,
	}
	if instrumented, ok := layerCache.(*instrumentedLayerCache); ok {
		run.cacheHits = instrumented.hits.Load()
		run.cachePuts = instrumented.puts.Load()
	}
	return run, nil
}

// uncompressedBytes sums the decompressed sizes of all layers, derived from
// each layer's final gzip checkpoint (recorded at end-of-stream).
func (r *indexRun) uncompressedBytes() int64 {
	info, ok := r.metadata.StorageInfo.(common.OCIStorageInfo)
	if !ok {
		return 0
	}
	var total int64
	for _, idx := range info.GzipIdxByLayer {
		if idx != nil && len(idx.Checkpoints) > 0 {
			total += idx.Checkpoints[len(idx.Checkpoints)-1].UOff
		}
	}
	return total
}

func (r *indexRun) layerCount() int {
	info, ok := r.metadata.StorageInfo.(common.OCIStorageInfo)
	if !ok {
		return 0
	}
	return len(info.Layers)
}

func (r *indexRun) throughput() string {
	secs := r.duration.Seconds()
	if secs <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f MiB/s", float64(r.uncompressedBytes())/(1<<20)/secs)
}

// compareRuns asserts two runs produced identical results. The index region
// must be byte-identical; storage info is compared by deep equality because
// gob encodes its maps in nondeterministic key order.
//
// Gzip checkpoint compressed offsets (COff) are normalized before comparison:
// they are measured through the gzip reader's input buffering, so their exact
// values depend on network read chunking and differ between independent cold
// runs against real registries. They are advisory seek hints (only consulted
// by the optional UseCheckpoints read path); the uncompressed offsets (UOff),
// which define checkpoint placement, are fully deterministic and compared
// exactly.
func compareRuns(a, b *indexRun) error {
	if !reflect.DeepEqual(a.indexBytes, b.indexBytes) {
		return fmt.Errorf("index bytes differ between %s (%d bytes) and %s (%d bytes)",
			a.label, len(a.indexBytes), b.label, len(b.indexBytes))
	}

	infoA := normalizeStorageInfo(a.metadata.StorageInfo)
	infoB := normalizeStorageInfo(b.metadata.StorageInfo)
	if !reflect.DeepEqual(infoA, infoB) {
		return fmt.Errorf("storage info differs between %s and %s: %s", a.label, b.label, describeStorageInfoDiff(infoA, infoB))
	}
	return nil
}

// normalizeStorageInfo zeroes the buffering-dependent compressed offsets in
// gzip checkpoints so comparisons cover only deterministic fields.
func normalizeStorageInfo(info interface{}) interface{} {
	ociInfo, ok := info.(common.OCIStorageInfo)
	if !ok {
		return info
	}

	normalized := ociInfo
	normalized.GzipIdxByLayer = make(map[string]*common.GzipIndex, len(ociInfo.GzipIdxByLayer))
	for digest, idx := range ociInfo.GzipIdxByLayer {
		if idx == nil {
			normalized.GzipIdxByLayer[digest] = nil
			continue
		}
		checkpoints := make([]common.GzipCheckpoint, len(idx.Checkpoints))
		for i, cp := range idx.Checkpoints {
			checkpoints[i] = common.GzipCheckpoint{UOff: cp.UOff}
		}
		normalized.GzipIdxByLayer[digest] = &common.GzipIndex{
			LayerDigest: idx.LayerDigest,
			Checkpoints: checkpoints,
		}
	}
	return normalized
}

// describeStorageInfoDiff returns a human-readable summary of which storage
// info fields differ between two runs.
func describeStorageInfoDiff(a, b interface{}) string {
	infoA, okA := a.(common.OCIStorageInfo)
	infoB, okB := b.(common.OCIStorageInfo)
	if !okA || !okB {
		return fmt.Sprintf("type mismatch: %T vs %T", a, b)
	}

	var diffs []string
	if infoA.RegistryURL != infoB.RegistryURL {
		diffs = append(diffs, fmt.Sprintf("RegistryURL: %q vs %q", infoA.RegistryURL, infoB.RegistryURL))
	}
	if infoA.Repository != infoB.Repository {
		diffs = append(diffs, fmt.Sprintf("Repository: %q vs %q", infoA.Repository, infoB.Repository))
	}
	if infoA.Reference != infoB.Reference {
		diffs = append(diffs, fmt.Sprintf("Reference: %q vs %q", infoA.Reference, infoB.Reference))
	}
	if !reflect.DeepEqual(infoA.Layers, infoB.Layers) {
		diffs = append(diffs, fmt.Sprintf("Layers: %v vs %v", infoA.Layers, infoB.Layers))
	}
	if !reflect.DeepEqual(infoA.DecompressedHashByLayer, infoB.DecompressedHashByLayer) {
		diffs = append(diffs, fmt.Sprintf("DecompressedHashByLayer: %v vs %v", infoA.DecompressedHashByLayer, infoB.DecompressedHashByLayer))
	}
	if !reflect.DeepEqual(infoA.GzipIdxByLayer, infoB.GzipIdxByLayer) {
		diffs = append(diffs, "GzipIdxByLayer differs")
	}
	if !reflect.DeepEqual(infoA.ImageMetadata, infoB.ImageMetadata) {
		diffs = append(diffs, "ImageMetadata differs")
	}
	if len(diffs) == 0 {
		return "(no field-level diff found)"
	}
	return fmt.Sprintf("%v", diffs)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
