package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
)

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose clip logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*verbose {
		clip.SetLogLevel("warn")
	}

	ctx := context.Background()

	workDir, err := os.MkdirTemp("", "clip-harness-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	fmt.Println("==> building synthetic image and starting in-memory registry")
	imageRef, shutdown, err := startSynthRegistry()
	if err != nil {
		return err
	}
	defer shutdown()
	fmt.Printf("    image: %s\n", imageRef)

	// Run 1+2: cold, sequential vs parallel (no cache)
	fmt.Println("==> run 1: cold index, sequential (concurrency=1)")
	seq, err := indexImage(ctx, "cold-sequential", imageRef, workDir, nil, 1)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, index=%d bytes\n", seq.duration, len(seq.indexBytes))

	fmt.Println("==> run 2: cold index, parallel (concurrency=8)")
	par, err := indexImage(ctx, "cold-parallel", imageRef, workDir, nil, 8)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, index=%d bytes\n", par.duration, len(par.indexBytes))

	if err := compareRuns(seq, par); err != nil {
		return fmt.Errorf("parallel indexing is not deterministic: %w", err)
	}
	fmt.Println("    OK: sequential and parallel runs are identical")

	// Run 3: populate the layer index cache
	diskCache, err := storage.NewDiskLayerIndexCache(workDir + "/layer-cache")
	if err != nil {
		return err
	}

	fmt.Println("==> run 3: cold index, populating layer index cache")
	populate := &countingLayerCache{inner: diskCache}
	pop, err := indexImage(ctx, "cache-populate", imageRef, workDir, populate, 8)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, cache hits=%d puts=%d\n", pop.duration, pop.cacheHits, pop.cachePuts)
	if pop.cacheHits != 0 {
		return fmt.Errorf("expected 0 cache hits on populate run, got %d", pop.cacheHits)
	}
	if pop.cachePuts == 0 {
		return fmt.Errorf("expected layer artifacts to be stored on populate run")
	}
	if err := compareRuns(seq, pop); err != nil {
		return fmt.Errorf("cache-populating run differs from cold run: %w", err)
	}

	// Run 4: warm — all layers must come from the cache (no pulls)
	fmt.Println("==> run 4: warm index from layer index cache")
	warmCounting := &countingLayerCache{inner: diskCache}
	warm, err := indexImage(ctx, "cache-warm", imageRef, workDir, warmCounting, 8)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, cache hits=%d puts=%d\n", warm.duration, warm.cacheHits, warm.cachePuts)
	if warm.cacheHits != pop.cachePuts {
		return fmt.Errorf("expected %d cache hits on warm run, got %d", pop.cachePuts, warm.cacheHits)
	}
	if warm.cachePuts != 0 {
		return fmt.Errorf("expected no cache puts on warm run, got %d", warm.cachePuts)
	}
	if err := compareRuns(seq, warm); err != nil {
		return fmt.Errorf("cache-warm run differs from cold run: %w", err)
	}
	fmt.Println("    OK: warm run identical to cold run, all layers served from cache")

	// Ground truth: compare decoded index against independently computed expectations
	fmt.Println("==> verifying index against independently computed ground truth")
	if err := verifyGroundTruth(warm.metadata); err != nil {
		return err
	}
	fmt.Println("    OK: index matches ground truth")

	fmt.Println("PASS: all verification checks succeeded")
	return nil
}

// verifyGroundTruth compares the decoded clip index with overlay semantics
// computed directly from the synthetic layer definitions.
func verifyGroundTruth(metadata *common.ClipArchiveMetadata) error {
	expected := computeGroundTruth()

	actual := map[string]*common.ClipNode{}
	metadata.Index.Ascend(metadata.Index.Min(), func(a interface{}) bool {
		node := a.(*common.ClipNode)
		actual[node.Path] = node
		return true
	})

	for _, p := range sortedPaths(expected) {
		want := expected[p]
		node, ok := actual[p]
		if !ok {
			return fmt.Errorf("ground truth mismatch: missing path %s", p)
		}
		switch want.kind {
		case "dir":
			if node.NodeType != common.DirNode {
				return fmt.Errorf("ground truth mismatch: %s should be dir, got %s", p, node.NodeType)
			}
		case "file":
			if node.NodeType != common.FileNode {
				return fmt.Errorf("ground truth mismatch: %s should be file, got %s", p, node.NodeType)
			}
			if int64(node.Attr.Size) != want.size {
				return fmt.Errorf("ground truth mismatch: %s size %d, want %d", p, node.Attr.Size, want.size)
			}
		case "symlink":
			if node.NodeType != common.SymLinkNode {
				return fmt.Errorf("ground truth mismatch: %s should be symlink, got %s", p, node.NodeType)
			}
			if node.Target != want.target {
				return fmt.Errorf("ground truth mismatch: %s target %q, want %q", p, node.Target, want.target)
			}
		}
	}

	for p := range actual {
		if _, ok := expected[p]; !ok {
			return fmt.Errorf("ground truth mismatch: unexpected path %s in index", p)
		}
	}

	if len(actual) != len(expected) {
		return fmt.Errorf("ground truth mismatch: %d paths in index, want %d", len(actual), len(expected))
	}
	return nil
}
