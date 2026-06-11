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

// countingLayerCache wraps a LayerIndexCache and counts hits/misses/puts.
type countingLayerCache struct {
	inner  storage.LayerIndexCache
	hits   atomic.Int64
	misses atomic.Int64
	puts   atomic.Int64
}

func (c *countingLayerCache) GetLayerIndex(ctx context.Context, key string) ([]byte, error) {
	data, err := c.inner.GetLayerIndex(ctx, key)
	if err == nil && data != nil {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return data, err
}

func (c *countingLayerCache) PutLayerIndex(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	return c.inner.PutLayerIndex(ctx, key, data)
}

// indexResult captures everything needed to compare two indexing runs.
type indexResult struct {
	label      string
	duration   time.Duration
	indexBytes []byte
	metadata   *common.ClipArchiveMetadata
	cacheHits  int64
	cachePuts  int64
}

// indexImage runs clip.CreateFromOCIImage and extracts the encoded index
// region plus decoded metadata from the resulting .clip file.
func indexImage(ctx context.Context, label, imageRef, outDir string, layerCache storage.LayerIndexCache, concurrency int) (*indexResult, error) {
	outputPath := filepath.Join(outDir, label+".clip")

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

	fileBytes, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, err
	}
	header := metadata.Header
	if header.IndexPos+header.IndexLength > int64(len(fileBytes)) {
		return nil, fmt.Errorf("invalid index region in %s", outputPath)
	}
	indexBytes := fileBytes[header.IndexPos : header.IndexPos+header.IndexLength]

	result := &indexResult{
		label:      label,
		duration:   duration,
		indexBytes: indexBytes,
		metadata:   metadata,
	}
	if counting, ok := layerCache.(*countingLayerCache); ok {
		result.cacheHits = counting.hits.Load()
		result.cachePuts = counting.puts.Load()
	}
	return result, nil
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
func compareRuns(a, b *indexResult) error {
	if len(a.indexBytes) != len(b.indexBytes) {
		return fmt.Errorf("index size mismatch: %s=%d bytes, %s=%d bytes", a.label, len(a.indexBytes), b.label, len(b.indexBytes))
	}
	for i := range a.indexBytes {
		if a.indexBytes[i] != b.indexBytes[i] {
			return fmt.Errorf("index bytes differ between %s and %s at offset %d", a.label, b.label, i)
		}
	}
	infoA := normalizeStorageInfo(a.metadata.StorageInfo)
	infoB := normalizeStorageInfo(b.metadata.StorageInfo)
	if !reflect.DeepEqual(infoA, infoB) {
		return fmt.Errorf("storage info differs between %s and %s: %s", a.label, b.label, diffStorageInfo(infoA, infoB))
	}
	return nil
}

// normalizeStorageInfo zeroes the buffering-dependent compressed offsets in
// gzip checkpoints so that comparisons cover only deterministic fields.
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
		cps := make([]common.GzipCheckpoint, len(idx.Checkpoints))
		for i, cp := range idx.Checkpoints {
			cps[i] = common.GzipCheckpoint{COff: 0, UOff: cp.UOff}
		}
		normalized.GzipIdxByLayer[digest] = &common.GzipIndex{
			LayerDigest: idx.LayerDigest,
			Checkpoints: cps,
		}
	}
	return normalized
}

func diffStorageInfo(a, b interface{}) string {
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
		for digest, idxA := range infoA.GzipIdxByLayer {
			idxB := infoB.GzipIdxByLayer[digest]
			if !reflect.DeepEqual(idxA, idxB) {
				diffs = append(diffs, fmt.Sprintf("GzipIdxByLayer[%s]: %d vs %d checkpoints", digest, checkpointCount(idxA), checkpointCount(idxB)))
			}
		}
		for digest := range infoB.GzipIdxByLayer {
			if _, ok := infoA.GzipIdxByLayer[digest]; !ok {
				diffs = append(diffs, fmt.Sprintf("GzipIdxByLayer[%s]: missing in first", digest))
			}
		}
	}
	if !reflect.DeepEqual(infoA.ImageMetadata, infoB.ImageMetadata) {
		diffs = append(diffs, fmt.Sprintf("ImageMetadata: %+v vs %+v", infoA.ImageMetadata, infoB.ImageMetadata))
	}
	if len(diffs) == 0 {
		return "(no field-level diff found; possible nil/empty mismatch)"
	}
	return fmt.Sprintf("%v", diffs)
}

func checkpointCount(idx *common.GzipIndex) int {
	if idx == nil {
		return -1
	}
	return len(idx.Checkpoints)
}
