package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/storage"
)

// runBench benchmarks indexing of a real image across three modes: cold
// sequential, cold parallel, and warm (layer index cache), verifying that all
// modes produce identical output.
func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	image := fs.String("image", "python:3.11-slim", "image reference to index")
	concurrency := fs.Int("concurrency", 4, "layer index concurrency")
	cacheDir := fs.String("cache-dir", "", "layer index cache dir (default: fresh temp dir)")
	verbose := fs.Bool("v", false, "verbose clip logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*verbose {
		clip.SetLogLevel("warn")
	}

	ctx := context.Background()

	workDir, err := os.MkdirTemp("", "clip-harness-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	cacheRoot := *cacheDir
	if cacheRoot == "" {
		cacheRoot = filepath.Join(workDir, "layer-cache")
	}
	diskCache, err := storage.NewDiskLayerIndexCache(cacheRoot)
	if err != nil {
		return err
	}

	fmt.Printf("==> benchmarking index of %s (concurrency=%d)\n", *image, *concurrency)

	fmt.Println("==> cold sequential run (no layer index cache, concurrency=1)")
	coldSequential, err := runIndex(ctx, "bench-cold-seq", *image, workDir, nil, 1)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (%s decompressed, %s)\n", coldSequential.duration, humanBytes(coldSequential.uncompressedBytes()), coldSequential.throughput())

	fmt.Println("==> cold parallel run (no layer index cache)")
	coldParallel, err := runIndex(ctx, "bench-cold", *image, workDir, nil, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (%s)\n", coldParallel.duration, coldParallel.throughput())

	fmt.Println("==> populate run (writes layer index cache)")
	populate, err := runIndex(ctx, "bench-populate", *image, workDir, &instrumentedLayerCache{inner: diskCache}, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (hits=%d puts=%d)\n", populate.duration, populate.cacheHits, populate.cachePuts)

	fmt.Println("==> warm run (layer index cache hits, no layer pulls)")
	warm, err := runIndex(ctx, "bench-warm", *image, workDir, &instrumentedLayerCache{inner: diskCache}, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (hits=%d puts=%d)\n", warm.duration, warm.cacheHits, warm.cachePuts)

	if err := compareRuns(coldSequential, coldParallel); err != nil {
		return fmt.Errorf("parallel cold run output differs from sequential cold run: %w", err)
	}
	if err := compareRuns(coldParallel, warm); err != nil {
		return fmt.Errorf("warm run output differs from cold run: %w", err)
	}

	fmt.Println()
	fmt.Println("results:")
	fmt.Printf("  image decompressed size: %s in %d layers\n", humanBytes(coldSequential.uncompressedBytes()), coldSequential.layerCount())
	fmt.Printf("  cold sequential: %12v  (%s)\n", coldSequential.duration, coldSequential.throughput())
	fmt.Printf("  cold parallel:   %12v  (%s, %.1fx vs sequential)\n",
		coldParallel.duration, coldParallel.throughput(), float64(coldSequential.duration)/float64(coldParallel.duration))
	fmt.Printf("  populate:        %12v\n", populate.duration)
	fmt.Printf("  warm:            %12v  (%.1fx vs cold sequential, %d/%d layers from cache)\n",
		warm.duration, float64(coldSequential.duration)/float64(warm.duration), warm.cacheHits, warm.cacheHits+warm.cachePuts)
	fmt.Println("  outputs verified identical across all runs")
	return nil
}
