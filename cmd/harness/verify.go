package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
)

// runVerify asserts that indexing the reference image is deterministic and
// correct across sequential, parallel, and layer-index-cached runs.
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

	fmt.Println("==> building reference image and starting in-memory registry")
	reg, err := startLocalRegistry()
	if err != nil {
		return err
	}
	defer reg.close()

	imageRef, err := pushReferenceImage(reg)
	if err != nil {
		return err
	}
	fmt.Printf("    image: %s\n", imageRef)

	fmt.Println("==> run 1: cold index, sequential (concurrency=1)")
	sequential, err := runIndex(ctx, "cold-sequential", imageRef, workDir, nil, 1)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, index=%d bytes\n", sequential.duration, len(sequential.indexBytes))

	fmt.Println("==> run 2: cold index, parallel (concurrency=8)")
	parallel, err := runIndex(ctx, "cold-parallel", imageRef, workDir, nil, 8)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, index=%d bytes\n", parallel.duration, len(parallel.indexBytes))

	if err := compareRuns(sequential, parallel); err != nil {
		return fmt.Errorf("parallel indexing is not deterministic: %w", err)
	}
	fmt.Println("    OK: sequential and parallel runs are identical")

	diskCache, err := storage.NewDiskLayerIndexCache(filepath.Join(workDir, "layer-cache"))
	if err != nil {
		return err
	}

	fmt.Println("==> run 3: cold index, populating layer index cache")
	populate, err := runIndex(ctx, "cache-populate", imageRef, workDir, &instrumentedLayerCache{inner: diskCache}, 8)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, cache hits=%d puts=%d\n", populate.duration, populate.cacheHits, populate.cachePuts)
	if populate.cacheHits != 0 {
		return fmt.Errorf("expected 0 cache hits on populate run, got %d", populate.cacheHits)
	}
	if populate.cachePuts == 0 {
		return fmt.Errorf("expected layer artifacts to be stored on populate run")
	}
	if err := compareRuns(sequential, populate); err != nil {
		return fmt.Errorf("cache-populating run differs from cold run: %w", err)
	}

	fmt.Println("==> run 4: warm index from layer index cache")
	warm, err := runIndex(ctx, "cache-warm", imageRef, workDir, &instrumentedLayerCache{inner: diskCache}, 8)
	if err != nil {
		return err
	}
	fmt.Printf("    %v, cache hits=%d puts=%d\n", warm.duration, warm.cacheHits, warm.cachePuts)
	if warm.cacheHits != populate.cachePuts {
		return fmt.Errorf("expected %d cache hits on warm run, got %d", populate.cachePuts, warm.cacheHits)
	}
	if warm.cachePuts != 0 {
		return fmt.Errorf("expected no cache puts on warm run, got %d", warm.cachePuts)
	}
	if err := compareRuns(sequential, warm); err != nil {
		return fmt.Errorf("cache-warm run differs from cold run: %w", err)
	}
	fmt.Println("    OK: warm run identical to cold run, all layers served from cache")

	fmt.Println("==> verifying index against independently computed ground truth")
	if err := verifyIndexAgainstExpectedTree(warm.metadata); err != nil {
		return err
	}
	fmt.Println("    OK: index matches ground truth")

	fmt.Println("PASS: all verification checks succeeded")
	return nil
}

// verifyIndexAgainstExpectedTree compares the decoded clip index with overlay
// semantics computed directly from the reference layer definitions.
func verifyIndexAgainstExpectedTree(metadata *common.ClipArchiveMetadata) error {
	expected := expectedTree()

	actual := make(map[string]*common.ClipNode, len(expected))
	metadata.Index.Ascend(metadata.Index.Min(), func(item interface{}) bool {
		node := item.(*common.ClipNode)
		actual[node.Path] = node
		return true
	})

	paths := make([]string, 0, len(expected))
	for p := range expected {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
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
	return nil
}
