package main

import (
	"context"
	"fmt"
	"os"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	log "github.com/rs/zerolog/log"
)

const (
	version = "2.0.0"
	defaultCheckpointMiB = 2
	defaultMountBase = "/var/lib/clip"
	defaultRootfsBase = "/run/clip"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "index":
		handleIndex(os.Args[2:])
	case "mount":
		handleMount(os.Args[2:])
	case "umount", "unmount":
		handleUmount(os.Args[2:])
	case "version":
		fmt.Printf("clipctl version %s\n", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`clipctl - Clip v2 OCI Image Manager

Usage:
  clipctl <command> [options]

Commands:
  index     Build metadata-only index file from an OCI image
  mount     Mount an OCI image and create rootfs for containers
  umount    Unmount and cleanup a container's rootfs
  version   Print version information
  help      Show this help message

Index Command:
  clipctl index --image <image-ref> --out <output-file> [options]

  Options:
    --image         OCI image reference (e.g., docker.io/library/python:3.12)
    --out           Output .clip file path
    --checkpoint    Checkpoint interval in MiB (default: 2)
    --verbose       Enable verbose logging

  Example:
    clipctl index --image docker.io/library/python:3.12 --out /var/lib/clip/indices/python:3.12.clip

Mount Command:
  clipctl mount --clip <clip-file> --cid <container-id> [options]

  Options:
    --clip          Path to .clip metadata file
    --cid           Container ID
    --mount-base    Base directory for mounts (default: /var/lib/clip)
    --rootfs-base   Base directory for rootfs (default: /run/clip)
    --kernel-overlay Use kernel overlayfs (default: true)
    --verbose       Enable verbose logging

  Example:
    clipctl mount --clip python.clip --cid abc123

Umount Command:
  clipctl umount --cid <container-id> [options]

  Options:
    --cid           Container ID
    --mount-base    Base directory for mounts (default: /var/lib/clip)
    --rootfs-base   Base directory for rootfs (default: /run/clip)

  Example:
    clipctl umount --cid abc123
`)
}

func handleIndex(args []string) {
	var imageRef, outputFile string
	var checkpointMiB int64 = defaultCheckpointMiB
	var verbose bool

	// Parse arguments
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--image":
			if i+1 < len(args) {
				imageRef = args[i+1]
				i++
			}
		case "--out":
			if i+1 < len(args) {
				outputFile = args[i+1]
				i++
			}
		case "--checkpoint":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &checkpointMiB)
				i++
			}
		case "--verbose":
			verbose = true
		}
	}

	// Validate required arguments
	if imageRef == "" {
		fmt.Fprintf(os.Stderr, "Error: --image is required\n")
		os.Exit(1)
	}
	if outputFile == "" {
		fmt.Fprintf(os.Stderr, "Error: --out is required\n")
		os.Exit(1)
	}

	// Create archiver
	archiver := clip.NewClipArchiver()

	// Index the OCI image
	log.Info().Msgf("Indexing OCI image: %s", imageRef)
	log.Info().Msgf("Checkpoint interval: %d MiB", checkpointMiB)

	ctx := context.Background()
	opts := clip.IndexOCIImageOptions{
		ImageRef:      imageRef,
		CheckpointMiB: checkpointMiB,
		Verbose:       verbose,
	}

	err := archiver.CreateFromOCI(ctx, opts, outputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating index: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Successfully created index: %s\n", outputFile)
	fmt.Printf("  Use 'clipctl mount' to mount this image\n")
}

func handleMount(args []string) {
	var clipFile, containerID, mountBase, rootfsBase string
	var useKernelOverlay bool = true
	var verbose bool

	// Parse arguments
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--clip":
			if i+1 < len(args) {
				clipFile = args[i+1]
				i++
			}
		case "--cid":
			if i+1 < len(args) {
				containerID = args[i+1]
				i++
			}
		case "--mount-base":
			if i+1 < len(args) {
				mountBase = args[i+1]
				i++
			}
		case "--rootfs-base":
			if i+1 < len(args) {
				rootfsBase = args[i+1]
				i++
			}
		case "--kernel-overlay":
			if i+1 < len(args) {
				useKernelOverlay = args[i+1] == "true"
				i++
			}
		case "--verbose":
			verbose = true
		}
	}

	// Validate required arguments
	if clipFile == "" {
		fmt.Fprintf(os.Stderr, "Error: --clip is required\n")
		os.Exit(1)
	}
	if containerID == "" {
		fmt.Fprintf(os.Stderr, "Error: --cid is required\n")
		os.Exit(1)
	}

	// Set defaults
	if mountBase == "" {
		mountBase = defaultMountBase
	}
	if rootfsBase == "" {
		rootfsBase = defaultRootfsBase
	}

	// Load metadata
	log.Info().Msgf("Loading clip file: %s", clipFile)
	archiver := clip.NewClipArchiver()
	metadata, err := archiver.ExtractMetadata(clipFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading clip file: %v\n", err)
		os.Exit(1)
	}

	// Create storage
	clipStorage, err := storage.NewClipStorage(storage.ClipStorageOpts{
		ArchivePath: clipFile,
		Metadata:    metadata,
		Credentials: storage.ClipStorageCredentials{},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating storage: %v\n", err)
		os.Exit(1)
	}

	// Extract image digest for mount point
	var imageDigest string
	if metadata.StorageInfo != nil {
		if ociInfo, ok := metadata.StorageInfo.(common.OCIStorageInfo); ok {
			imageDigest = ociInfo.Reference
		} else if ociInfo, ok := metadata.StorageInfo.(*common.OCIStorageInfo); ok {
			imageDigest = ociInfo.Reference
		}
	}

	// Create overlay mounter
	mounter := clip.NewOverlayMounter(clip.OverlayMountOptions{
		ContainerID:        containerID,
		ImageDigest:        imageDigest,
		MountBase:          mountBase,
		RootfsBase:         rootfsBase,
		UseKernelOverlayfs: useKernelOverlay,
	})

	// Mount
	log.Info().Msgf("Mounting container: %s", containerID)
	ctx := context.Background()
	mount, err := mounter.Mount(ctx, clipStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error mounting: %v\n", err)
		os.Exit(1)
	}

	// Print rootfs path
	fmt.Printf("%s\n", mount.RootfsPath)
	
	if verbose {
		fmt.Fprintf(os.Stderr, "✓ Mounted successfully\n")
		fmt.Fprintf(os.Stderr, "  RO mount: %s\n", mount.ROMount)
		fmt.Fprintf(os.Stderr, "  Upper dir: %s\n", mount.UpperDir)
		fmt.Fprintf(os.Stderr, "  Work dir: %s\n", mount.WorkDir)
		fmt.Fprintf(os.Stderr, "  Rootfs: %s\n", mount.RootfsPath)
	}
}

func handleUmount(args []string) {
	var containerID, mountBase, rootfsBase string

	// Parse arguments
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cid":
			if i+1 < len(args) {
				containerID = args[i+1]
				i++
			}
		case "--mount-base":
			if i+1 < len(args) {
				mountBase = args[i+1]
				i++
			}
		case "--rootfs-base":
			if i+1 < len(args) {
				rootfsBase = args[i+1]
				i++
			}
		}
	}

	// Validate required arguments
	if containerID == "" {
		fmt.Fprintf(os.Stderr, "Error: --cid is required\n")
		os.Exit(1)
	}

	// Set defaults
	if mountBase == "" {
		mountBase = defaultMountBase
	}
	if rootfsBase == "" {
		rootfsBase = defaultRootfsBase
	}

	// Create overlay mounter
	mounter := clip.NewOverlayMounter(clip.OverlayMountOptions{
		ContainerID: containerID,
		MountBase:   mountBase,
		RootfsBase:  rootfsBase,
	})

	// Reconstruct mount info
	mount := &clip.OverlayMount{
		ContainerID: containerID,
		RootfsPath:  mounter.GetRootfsPath(containerID),
	}
	mount.ROMount = mountBase + "/mnts/*/ro" // Will try to unmount
	mount.UpperDir = mountBase + "/upper/" + containerID
	mount.WorkDir = mountBase + "/work/" + containerID

	// Cleanup
	log.Info().Msgf("Unmounting container: %s", containerID)
	err := mounter.Cleanup(mount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cleanup errors: %v\n", err)
		// Don't exit with error - partial cleanup is okay
	}

	fmt.Printf("✓ Unmounted container: %s\n", containerID)
}
