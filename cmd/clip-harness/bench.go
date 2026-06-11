package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/storage"
)

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
		cacheRoot = workDir + "/layer-cache"
	}
	diskCache, err := storage.NewDiskLayerIndexCache(cacheRoot)
	if err != nil {
		return err
	}

	fmt.Printf("==> benchmarking index of %s (concurrency=%d)\n", *image, *concurrency)

	fmt.Println("==> cold run (no layer index cache)")
	cold, err := indexImage(ctx, "bench-cold", *image, workDir, nil, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v\n", cold.duration)

	fmt.Println("==> populate run (writes layer index cache)")
	popCache := &countingLayerCache{inner: diskCache}
	pop, err := indexImage(ctx, "bench-populate", *image, workDir, popCache, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (hits=%d puts=%d)\n", pop.duration, pop.cacheHits, pop.cachePuts)

	fmt.Println("==> warm run (layer index cache hits, no layer pulls)")
	warmCache := &countingLayerCache{inner: diskCache}
	warm, err := indexImage(ctx, "bench-warm", *image, workDir, warmCache, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (hits=%d puts=%d)\n", warm.duration, warm.cacheHits, warm.cachePuts)

	if err := compareRuns(cold, warm); err != nil {
		return fmt.Errorf("warm run output differs from cold run: %w", err)
	}

	speedup := float64(cold.duration) / float64(warm.duration)
	fmt.Println()
	fmt.Println("results:")
	fmt.Printf("  cold:       %12v\n", cold.duration)
	fmt.Printf("  populate:   %12v\n", pop.duration)
	fmt.Printf("  warm:       %12v  (%.1fx faster than cold, %d/%d layers from cache)\n",
		warm.duration, speedup, warm.cacheHits, warm.cacheHits+warm.cachePuts)
	fmt.Println("  warm output verified identical to cold output")
	return nil
}
