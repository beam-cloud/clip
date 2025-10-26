package clip

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// OverlayManager manages overlay mounts for containers
type OverlayManager struct {
	baseDir string // Base directory for overlay mounts (e.g., /var/lib/clip)
}

// OverlayMountOptions contains options for overlay mounting
type OverlayMountOptions struct {
	ImageDigest string // Image digest for stable paths
	ContainerID string // Container ID
	ReadOnlyDir string // Path to read-only FUSE mount
	RootfsPath  string // Target rootfs path for container runtime
}

// NewOverlayManager creates a new overlay manager
func NewOverlayManager(baseDir string) *OverlayManager {
	if baseDir == "" {
		baseDir = "/var/lib/clip"
	}
	return &OverlayManager{
		baseDir: baseDir,
	}
}

// SetupOverlayMount creates an overlay mount for a container
func (om *OverlayManager) SetupOverlayMount(opts OverlayMountOptions) error {
	log.Info().Msgf("setting up overlay mount for container %s", opts.ContainerID)

	// Create necessary directories
	if err := om.createDirectories(opts); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// Try kernel overlayfs first, fallback to fuse-overlayfs
	if err := om.tryKernelOverlay(opts); err != nil {
		log.Warn().Err(err).Msg("kernel overlayfs failed, trying fuse-overlayfs")
		if err := om.tryFuseOverlay(opts); err != nil {
			return fmt.Errorf("both kernel and fuse overlayfs failed: %w", err)
		}
	}

	log.Info().Msgf("overlay mount ready at %s", opts.RootfsPath)
	return nil
}

// CleanupOverlayMount removes an overlay mount and cleans up directories
func (om *OverlayManager) CleanupOverlayMount(containerID string) error {
	log.Info().Msgf("cleaning up overlay mount for container %s", containerID)

	rootfsPath := filepath.Join("/run/clip", containerID, "rootfs")
	
	// Unmount if mounted
	if om.isMounted(rootfsPath) {
		if err := om.unmount(rootfsPath); err != nil {
			log.Warn().Err(err).Msg("failed to unmount rootfs")
		}
	}

	// Clean up directories
	containerDir := filepath.Join(om.baseDir, "containers", containerID)
	if err := os.RemoveAll(containerDir); err != nil {
		log.Warn().Err(err).Msg("failed to remove container directory")
	}

	runDir := filepath.Join("/run/clip", containerID)
	if err := os.RemoveAll(runDir); err != nil {
		log.Warn().Err(err).Msg("failed to remove run directory")
	}

	return nil
}

// createDirectories creates all necessary directories for overlay mount
func (om *OverlayManager) createDirectories(opts OverlayMountOptions) error {
	dirs := []string{
		// Base directories
		om.baseDir,
		filepath.Join(om.baseDir, "containers"),
		filepath.Join(om.baseDir, "containers", opts.ContainerID),
		filepath.Join(om.baseDir, "containers", opts.ContainerID, "upper"),
		filepath.Join(om.baseDir, "containers", opts.ContainerID, "work"),
		
		// Runtime directories
		"/run/clip",
		filepath.Join("/run/clip", opts.ContainerID),
		filepath.Dir(opts.RootfsPath),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// tryKernelOverlay attempts to use kernel overlayfs
func (om *OverlayManager) tryKernelOverlay(opts OverlayMountOptions) error {
	upperDir := filepath.Join(om.baseDir, "containers", opts.ContainerID, "upper")
	workDir := filepath.Join(om.baseDir, "containers", opts.ContainerID, "work")

	// Check if overlayfs is supported
	if !om.isOverlayfsSupported() {
		return fmt.Errorf("kernel overlayfs not supported")
	}

	// Mount overlay
	mountOptions := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", 
		opts.ReadOnlyDir, upperDir, workDir)

	cmd := exec.Command("mount", "-t", "overlay", "overlay", 
		"-o", mountOptions, opts.RootfsPath)
	
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kernel overlay mount failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// tryFuseOverlay attempts to use fuse-overlayfs
func (om *OverlayManager) tryFuseOverlay(opts OverlayMountOptions) error {
	upperDir := filepath.Join(om.baseDir, "containers", opts.ContainerID, "upper")
	workDir := filepath.Join(om.baseDir, "containers", opts.ContainerID, "work")

	// Check if fuse-overlayfs is available
	if _, err := exec.LookPath("fuse-overlayfs"); err != nil {
		return fmt.Errorf("fuse-overlayfs not found in PATH")
	}

	// Mount with fuse-overlayfs
	cmd := exec.Command("fuse-overlayfs",
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", 
			opts.ReadOnlyDir, upperDir, workDir),
		opts.RootfsPath)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fuse-overlayfs mount failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// isOverlayfsSupported checks if kernel overlayfs is supported
func (om *OverlayManager) isOverlayfsSupported() bool {
	// Check if overlayfs module is loaded or built-in
	if _, err := os.Stat("/proc/filesystems"); err != nil {
		return false
	}

	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}

	return strings.Contains(string(data), "overlay")
}

// isMounted checks if a path is mounted
func (om *OverlayManager) isMounted(path string) bool {
	// Check /proc/mounts
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}

	return false
}

// unmount unmounts a path
func (om *OverlayManager) unmount(path string) error {
	cmd := exec.Command("umount", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unmount failed: %w (output: %s)", err, string(output))
	}
	return nil
}

// GetRootfsPath returns the rootfs path for a container
func (om *OverlayManager) GetRootfsPath(containerID string) string {
	return filepath.Join("/run/clip", containerID, "rootfs")
}

// GetReadOnlyMountPath returns the read-only mount path for an image
func (om *OverlayManager) GetReadOnlyMountPath(imageDigest string) string {
	// Clean digest for use in filesystem path
	cleanDigest := strings.ReplaceAll(imageDigest, ":", "_")
	return filepath.Join(om.baseDir, "mounts", cleanDigest, "ro")
}

// MountReadOnly mounts a clip archive as read-only FUSE filesystem
func (om *OverlayManager) MountReadOnly(archivePath, imageDigest string) (string, error) {
	mountPath := om.GetReadOnlyMountPath(imageDigest)
	
	// Create mount directory
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create mount directory: %w", err)
	}

	// Check if already mounted
	if om.isMounted(mountPath) {
		log.Info().Msgf("read-only mount already exists at %s", mountPath)
		return mountPath, nil
	}

	// Mount the clip archive
	mountOpts := MountOptions{
		ArchivePath: archivePath,
		MountPoint:  mountPath,
		Verbose:     false,
	}

	startServer, serverError, server, err := MountArchive(mountOpts)
	if err != nil {
		return "", fmt.Errorf("failed to mount archive: %w", err)
	}

	// Start the FUSE server
	if err := startServer(); err != nil {
		return "", fmt.Errorf("failed to start FUSE server: %w", err)
	}

	// Wait for mount to be ready
	if err := server.WaitMount(); err != nil {
		return "", fmt.Errorf("failed to wait for mount: %w", err)
	}

	// Start monitoring server errors in background
	go func() {
		for err := range serverError {
			if err != nil {
				log.Error().Err(err).Msg("FUSE server error")
			}
		}
	}()

	log.Info().Msgf("read-only mount ready at %s", mountPath)
	return mountPath, nil
}

// UnmountReadOnly unmounts a read-only FUSE filesystem
func (om *OverlayManager) UnmountReadOnly(imageDigest string) error {
	mountPath := om.GetReadOnlyMountPath(imageDigest)
	
	if !om.isMounted(mountPath) {
		return nil // Already unmounted
	}

	return om.unmount(mountPath)
}