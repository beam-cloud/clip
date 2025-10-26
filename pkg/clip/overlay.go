package clip

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
)

// OverlayMountOptions configures overlay mount behavior
type OverlayMountOptions struct {
	ContainerID  string
	ImageDigest  string
	MountBase    string // Base directory for mounts (default: /var/lib/clip)
	RootfsBase   string // Base directory for rootfs (default: /run/clip)
	UseKernelOverlayfs bool // Prefer kernel overlayfs over fuse-overlayfs
	FUSEMountOpts []string // Additional FUSE mount options
}

// OverlayMount represents a mounted overlay filesystem
type OverlayMount struct {
	ContainerID string
	ROMount     string // Read-only FUSE mount point
	UpperDir    string
	WorkDir     string
	RootfsPath  string // Final merged rootfs path
	fuseServer  *fuse.Server
	fuseMounted bool
	overlayMounted bool
}

// OverlayMounter manages overlay mounts for containers
type OverlayMounter struct {
	opts OverlayMountOptions
}

func NewOverlayMounter(opts OverlayMountOptions) *OverlayMounter {
	// Set defaults
	if opts.MountBase == "" {
		opts.MountBase = "/var/lib/clip"
	}
	if opts.RootfsBase == "" {
		opts.RootfsBase = "/run/clip"
	}
	
	return &OverlayMounter{opts: opts}
}

// Mount creates a read-only FUSE mount and overlays it with a writable layer
func (om *OverlayMounter) Mount(ctx context.Context, clipStorage storage.ClipStorageInterface) (*OverlayMount, error) {
	mount := &OverlayMount{
		ContainerID: om.opts.ContainerID,
	}

	// Create mount directories
	if err := om.createMountDirs(mount); err != nil {
		return nil, fmt.Errorf("failed to create mount directories: %w", err)
	}

	// Mount FUSE filesystem (read-only)
	if err := om.mountFUSE(ctx, mount, clipStorage); err != nil {
		om.Cleanup(mount)
		return nil, fmt.Errorf("failed to mount FUSE filesystem: %w", err)
	}

	// Create overlay
	if err := om.createOverlay(mount); err != nil {
		om.Cleanup(mount)
		return nil, fmt.Errorf("failed to create overlay: %w", err)
	}

	log.Info().Msgf("Successfully created rootfs at: %s", mount.RootfsPath)
	return mount, nil
}

// createMountDirs creates all necessary directories for the mount
func (om *OverlayMounter) createMountDirs(mount *OverlayMount) error {
	// RO mount point: /var/lib/clip/mnts/<image-digest>/ro
	if om.opts.ImageDigest == "" {
		om.opts.ImageDigest = "default"
	}
	mount.ROMount = filepath.Join(om.opts.MountBase, "mnts", om.opts.ImageDigest, "ro")
	
	// Upper and work dirs for this container
	mount.UpperDir = filepath.Join(om.opts.MountBase, "upper", om.opts.ContainerID)
	mount.WorkDir = filepath.Join(om.opts.MountBase, "work", om.opts.ContainerID)
	
	// Final rootfs path
	mount.RootfsPath = filepath.Join(om.opts.RootfsBase, om.opts.ContainerID, "rootfs")

	// Create all directories
	dirs := []string{
		mount.ROMount,
		mount.UpperDir,
		mount.WorkDir,
		filepath.Dir(mount.RootfsPath),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// mountFUSE mounts the ClipFS filesystem at the RO mount point
func (om *OverlayMounter) mountFUSE(ctx context.Context, mount *OverlayMount, clipStorage storage.ClipStorageInterface) error {
	// Create ClipFileSystem
	cfs, err := NewFileSystem(clipStorage, ClipFileSystemOpts{
		Verbose:               false,
		ContentCacheAvailable: false,
	})
	if err != nil {
		return fmt.Errorf("failed to create clip filesystem: %w", err)
	}

	// Mount options
	mountOpts := &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:          "clipfs",
			FsName:        "clipfs",
			DisableXAttrs: false,
			Debug:         false,
			AllowOther:    true, // Allow other users (needed for containers)
			Options:       []string{"ro", "nodev", "nosuid", "noatime"},
		},
	}

	// Add custom options
	if len(om.opts.FUSEMountOpts) > 0 {
		mountOpts.MountOptions.Options = append(mountOpts.MountOptions.Options, om.opts.FUSEMountOpts...)
	}

	// Get the root node
	root, err := cfs.Root()
	if err != nil {
		return fmt.Errorf("failed to get root: %w", err)
	}

	// Mount the filesystem
	server, err := fs.Mount(mount.ROMount, root, mountOpts)
	if err != nil {
		return fmt.Errorf("failed to mount FUSE filesystem: %w", err)
	}

	mount.fuseServer = server
	mount.fuseMounted = true

	// Start serving in background
	go server.Serve()

	// Wait for mount to be ready
	if err := server.WaitMount(); err != nil {
		return fmt.Errorf("failed to wait for mount: %w", err)
	}

	// CRITICAL: Give FUSE mount time to stabilize before creating overlay
	// Without this, overlayfs may see a "deleted directory" error
	time.Sleep(500 * time.Millisecond)
	
	// Verify mount is accessible by listing root
	entries, err := os.ReadDir(mount.ROMount)
	if err != nil {
		return fmt.Errorf("FUSE mount not accessible: %w", err)
	}
	log.Info().Msgf("FUSE mounted at: %s (%d entries)", mount.ROMount, len(entries))
	
	return nil
}

// createOverlay creates an overlay filesystem with the FUSE mount as lower layer
func (om *OverlayMounter) createOverlay(mount *OverlayMount) error {
	// Try kernel overlayfs first if preferred and available
	if om.opts.UseKernelOverlayfs {
		if kernelErr := om.tryKernelOverlayfs(mount); kernelErr == nil {
			mount.overlayMounted = true
			return nil
		} else {
			log.Warn().Msgf("Kernel overlayfs failed, falling back to fuse-overlayfs: %v", kernelErr)
		}
	}

	// Fallback to fuse-overlayfs
	if err := om.tryFuseOverlayfs(mount); err != nil {
		return fmt.Errorf("both kernel and fuse overlayfs failed: %w", err)
	}

	mount.overlayMounted = true
	return nil
}

// tryKernelOverlayfs attempts to use kernel overlayfs
func (om *OverlayMounter) tryKernelOverlayfs(mount *OverlayMount) error {
	// Check if overlayfs is supported
	if !om.isOverlayfsSupported() {
		return fmt.Errorf("kernel overlayfs not supported")
	}

	// IMPORTANT: Verify lower dir exists and is accessible
	if _, err := os.Stat(mount.ROMount); err != nil {
		return fmt.Errorf("lower dir not accessible: %w", err)
	}

	// Use index=off and metacopy=off for better FUSE compatibility
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,index=off,metacopy=off",
		mount.ROMount,
		mount.UpperDir,
		mount.WorkDir,
	)

	// Mount using syscall with MS_NODEV|MS_NOSUID for security
	flags := uintptr(syscall.MS_NODEV | syscall.MS_NOSUID)
	err := syscall.Mount("overlay", mount.RootfsPath, "overlay", flags, opts)
	if err != nil {
		return fmt.Errorf("kernel overlayfs mount failed: %w", err)
	}

	// Verify the overlay mount is accessible
	if _, err := os.Stat(filepath.Join(mount.RootfsPath, ".")); err != nil {
		syscall.Unmount(mount.RootfsPath, syscall.MNT_FORCE)
		return fmt.Errorf("overlay mount not accessible after creation: %w", err)
	}

	log.Info().Msgf("Kernel overlayfs mounted at: %s", mount.RootfsPath)
	return nil
}

// tryFuseOverlayfs attempts to use fuse-overlayfs
func (om *OverlayMounter) tryFuseOverlayfs(mount *OverlayMount) error {
	// Check if fuse-overlayfs is available
	if _, err := exec.LookPath("fuse-overlayfs"); err != nil {
		return fmt.Errorf("fuse-overlayfs not found in PATH: %w", err)
	}

	cmd := exec.Command("fuse-overlayfs",
		"-o", fmt.Sprintf("lowerdir=%s", mount.ROMount),
		"-o", fmt.Sprintf("upperdir=%s", mount.UpperDir),
		"-o", fmt.Sprintf("workdir=%s", mount.WorkDir),
		mount.RootfsPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fuse-overlayfs failed: %w (output: %s)", err, string(output))
	}

	log.Info().Msgf("fuse-overlayfs mounted at: %s", mount.RootfsPath)
	return nil
}

// isOverlayfsSupported checks if kernel overlayfs is supported
func (om *OverlayMounter) isOverlayfsSupported() bool {
	// Try to read /proc/filesystems to check for overlay support
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.Contains(line, "overlay") {
			return true
		}
	}

	return false
}

// Cleanup unmounts and removes all mount points
func (om *OverlayMounter) Cleanup(mount *OverlayMount) error {
	var errors []error

	// Unmount overlay
	if mount.overlayMounted {
		if err := om.unmountOverlay(mount); err != nil {
			errors = append(errors, fmt.Errorf("failed to unmount overlay: %w", err))
		}
	}

	// Unmount FUSE
	if mount.fuseMounted {
		if err := om.unmountFUSE(mount); err != nil {
			errors = append(errors, fmt.Errorf("failed to unmount FUSE: %w", err))
		}
	}

	// Remove directories (optional, comment out to keep for debugging)
	// os.RemoveAll(mount.UpperDir)
	// os.RemoveAll(mount.WorkDir)
	// os.RemoveAll(filepath.Dir(mount.RootfsPath))

	if len(errors) > 0 {
		return fmt.Errorf("cleanup errors: %v", errors)
	}

	log.Info().Msgf("Cleanup completed for container: %s", mount.ContainerID)
	return nil
}

// unmountOverlay unmounts the overlay filesystem
func (om *OverlayMounter) unmountOverlay(mount *OverlayMount) error {
	// Try graceful unmount first
	err := syscall.Unmount(mount.RootfsPath, 0)
	if err != nil {
		// Try lazy unmount
		err = syscall.Unmount(mount.RootfsPath, syscall.MNT_DETACH)
		if err != nil {
			return fmt.Errorf("failed to unmount overlay at %s: %w", mount.RootfsPath, err)
		}
	}

	log.Info().Msgf("Overlay unmounted: %s", mount.RootfsPath)
	return nil
}

// unmountFUSE unmounts the FUSE filesystem
func (om *OverlayMounter) unmountFUSE(mount *OverlayMount) error {
	if mount.fuseServer != nil {
		if err := mount.fuseServer.Unmount(); err != nil {
			return fmt.Errorf("failed to unmount FUSE at %s: %w", mount.ROMount, err)
		}
		log.Info().Msgf("FUSE unmounted: %s", mount.ROMount)
	}
	return nil
}

// GetRootfsPath returns the rootfs path for a container
func (om *OverlayMounter) GetRootfsPath(containerID string) string {
	return filepath.Join(om.opts.RootfsBase, containerID, "rootfs")
}
