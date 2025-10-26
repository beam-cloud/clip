// Test indexing ubuntu image and checking symlinks
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
)

func main() {
	ctx := context.Background()
	archiver := clip.NewClipArchiver()

	// Index ubuntu:24.04
	fmt.Println("Indexing ubuntu:24.04...")
	clipPath := "/tmp/ubuntu-test.clip"
	defer os.Remove(clipPath)

	err := archiver.CreateFromOCI(ctx, clip.IndexOCIImageOptions{
		ImageRef:      "docker.io/library/ubuntu:24.04",
		CheckpointMiB: 2,
		Verbose:       false, // Turn on to see symlink details
	}, clipPath)
	if err != nil {
		log.Fatal("Failed to index:", err)
	}

	fmt.Println("\nLoading index back...")
	metadata, err := archiver.ExtractMetadata(clipPath)
	if err != nil {
		log.Fatal("Failed to load metadata:", err)
	}

	// Check symlinks
	fmt.Println("\nChecking symlinks in index:")
	symlinkCount := 0
	emptyTargetCount := 0

	metadata.Index.Ascend(metadata.Index.Min(), func(item interface{}) bool {
		node := item.(*common.ClipNode)
		if node.NodeType == common.SymLinkNode {
			symlinkCount++
			if node.Target == "" {
				emptyTargetCount++
				fmt.Printf("  EMPTY: %s -> '%s'\n", node.Path, node.Target)
			} else if symlinkCount <= 10 { // Show first 10
				fmt.Printf("  OK: %s -> %s\n", node.Path, node.Target)
			}
		}
		return true
	})

	fmt.Printf("\nTotal symlinks: %d\n", symlinkCount)
	fmt.Printf("Empty targets: %d\n", emptyTargetCount)

	if emptyTargetCount > 0 {
		log.Fatal("ERROR: Found symlinks with empty targets!")
	} else {
		fmt.Println("\n✓ All symlinks have valid targets")
	}
}
