// Debug script to diagnose FUSE mount issues
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/storage"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run debug_mount.go <clip-file>")
	}

	clipFile := os.Args[1]
	mountPoint := "/tmp/debug-mount"

	os.MkdirAll(mountPoint, 0755)
	defer os.RemoveAll(mountPoint)

	fmt.Printf("Loading clip file: %s\n", clipFile)
	
	archiver := clip.NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	if err != nil {
		log.Fatal("Failed to load metadata:", err)
	}

	fmt.Printf("Metadata loaded:\n")
	fmt.Printf("  Storage type: %s\n", metadata.StorageInfo.Type())
	fmt.Printf("  Index entries: %d\n", metadata.Index.Len())

	// Check for key directories
	checkDirs := []string{"/", "/proc", "/dev", "/sys", "/etc", "/bin", "/usr"}
	fmt.Printf("\nChecking index for critical directories:\n")
	for _, dir := range checkDirs {
		node := metadata.Get(dir)
		if node == nil {
			fmt.Printf("  ❌ MISSING: %s\n", dir)
		} else {
			fmt.Printf("  ✓ Found: %s (type=%s)\n", dir, node.NodeType)
		}
	}

	// Create storage
	clipStorage, err := storage.NewClipStorage(storage.ClipStorageOpts{
		ArchivePath: clipFile,
		Metadata:    metadata,
	})
	if err != nil {
		log.Fatal("Failed to create storage:", err)
	}

	fmt.Printf("\nMounting at: %s\n", mountPoint)
	
	startServer, serverError, server, err := clip.MountArchive(clip.MountOptions{
		ArchivePath: clipFile,
		MountPoint:  mountPoint,
		Verbose:     true,
	})
	if err != nil {
		log.Fatal("Failed to create mount:", err)
	}

	err = startServer()
	if err != nil {
		log.Fatal("Failed to start server:", err)
	}

	// Monitor for errors
	go func() {
		err := <-serverError
		if err != nil {
			log.Printf("Server error: %v", err)
		}
	}()

	// Give it a moment
	time.Sleep(100 * time.Millisecond)

	// Test the mount
	fmt.Printf("\nTesting mounted filesystem:\n")
	
	// 1. List root
	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		fmt.Printf("  ❌ Can't read root: %v\n", err)
	} else {
		fmt.Printf("  ✓ Root dir readable (%d entries)\n", len(entries))
	}

	// 2. Check critical directories
	for _, dir := range checkDirs {
		path := filepath.Join(mountPoint, dir)
		info, err := os.Lstat(path)
		if err != nil {
			fmt.Printf("  ❌ Can't stat %s: %v\n", dir, err)
		} else {
			fmt.Printf("  ✓ %s: mode=%o size=%d\n", dir, info.Mode(), info.Size())
			
			// If symlink, check target
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					fmt.Printf("    ❌ Can't read symlink: %v\n", err)
				} else if target == "" {
					fmt.Printf("    ❌ EMPTY symlink target!\n")
				} else {
					fmt.Printf("    ✓ Symlink target: %s\n", target)
				}
			}
		}
	}

	// 3. Try reading a file
	fmt.Printf("\nTrying to read /etc/os-release:\n")
	data, err := os.ReadFile(filepath.Join(mountPoint, "etc/os-release"))
	if err != nil {
		fmt.Printf("  ❌ Can't read file: %v\n", err)
		
		// Check if file exists in index
		node := metadata.Get("/etc/os-release")
		if node == nil {
			fmt.Printf("  File not in index!\n")
		} else {
			fmt.Printf("  File in index: size=%d remote=%v\n", node.Attr.Size, node.Remote != nil)
			if node.Remote != nil {
				fmt.Printf("    LayerDigest: %s\n", node.Remote.LayerDigest)
				fmt.Printf("    UOffset: %d\n", node.Remote.UOffset)
				fmt.Printf("    ULength: %d\n", node.Remote.ULength)
			}
		}
	} else {
		fmt.Printf("  ✓ Read %d bytes\n", len(data))
		fmt.Printf("  Content preview: %s...\n", string(data[:min(50, len(data))]))
	}

	fmt.Printf("\nMount test complete. Press Ctrl+C to exit.\n")
	server.Wait()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
