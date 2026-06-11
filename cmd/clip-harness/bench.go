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

	fmt.Println("==> cold sequential run (no layer index cache, concurrency=1)")
	coldSeq, err := indexImage(ctx, "bench-cold-seq", *image, workDir, nil, 1)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (%s decompressed, %s)\n", coldSeq.duration, humanBytes(coldSeq.uncompressedBytes()), throughput(coldSeq))

	fmt.Println("==> cold parallel run (no layer index cache)")
	cold, err := indexImage(ctx, "bench-cold", *image, workDir, nil, *concurrency)
	if err != nil {
		return err
	}
	fmt.Printf("    %v (%s)\n", cold.duration, throughput(cold))

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

	if err := compareRuns(coldSeq, cold); err != nil {
		return fmt.Errorf("parallel cold run output differs from sequential cold run: %w", err)
	}
	if err := compareRuns(cold, warm); err != nil {
		return fmt.Errorf("warm run output differs from cold run: %w", err)
	}

	fmt.Println()
	fmt.Println("results:")
	fmt.Printf("  image decompressed size: %s in %d layers\n", humanBytes(coldSeq.uncompressedBytes()), coldSeq.layerCount())
	fmt.Printf("  cold sequential: %12v  (%s)\n", coldSeq.duration, throughput(coldSeq))
	fmt.Printf("  cold parallel:   %12v  (%s, %.1fx vs sequential)\n", cold.duration, throughput(cold), float64(coldSeq.duration)/float64(cold.duration))
	fmt.Printf("  populate:        %12v\n", pop.duration)
	fmt.Printf("  warm:            %12v  (%.1fx vs cold sequential, %d/%d layers from cache)\n",
		warm.duration, float64(coldSeq.duration)/float64(warm.duration), warm.cacheHits, warm.cacheHits+warm.cachePuts)
	fmt.Println("  outputs verified identical across all runs")
	return nil
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

func throughput(r *indexResult) string {
	secs := r.duration.Seconds()
	if secs <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f MiB/s", float64(r.uncompressedBytes())/(1<<20)/secs)
}
