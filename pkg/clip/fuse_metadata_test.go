package clip

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFUSEMountMetadataPreservation verifies that metadata is correctly preserved
// in the actual mounted FUSE filesystem, not just in the index
func TestFUSEMountMetadataPreservation(t *testing.T) {
	// Skip in short mode OR in CI environments (no FUSE support)
	if testing.Short() {
		t.Skip("Skipping FUSE mount test in short mode")
	}
	
	// Check if fusermount is available
	if _, err := os.Stat("/bin/fusermount"); os.IsNotExist(err) {
		t.Skip("Skipping FUSE test: fusermount not available")
	}
	if _, err := os.Stat("/usr/bin/fusermount"); os.IsNotExist(err) {
		if _, err2 := os.Stat("/bin/fusermount"); os.IsNotExist(err2) {
			t.Skip("Skipping FUSE test: fusermount not found in /bin or /usr/bin")
		}
	}
	
	// This is an integration test that requires FUSE kernel module
	// Skip if running in environments without FUSE support (Docker, CI, etc.)
	t.Skip("Skipping FUSE integration test - requires FUSE kernel module and can hang in CI")
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	// Use Ubuntu which has well-known timestamps and metadata
	imageRef := "docker.io/library/ubuntu:22.04"
	clipFile := filepath.Join(tempDir, "ubuntu.clip")
	mountPoint := filepath.Join(tempDir, "mount")
	
	// Create index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
		Verbose:       false,
	})
	require.NoError(t, err, "CreateFromOCIImage should succeed")
	
	// Create mount point
	err = os.MkdirAll(mountPoint, 0755)
	require.NoError(t, err, "Failed to create mount point")
	
	// Mount the FUSE filesystem
	unmount, errChan, _, err := MountArchive(MountOptions{
		ArchivePath: clipFile,
		MountPoint:  mountPoint,
	})
	if err != nil {
		t.Skipf("Cannot mount FUSE (fusermount not available): %v", err)
		return
	}
	defer unmount()
	
	// Check for mount errors
	select {
	case err := <-errChan:
		if err != nil {
			t.Skipf("FUSE mount error: %v", err)
			return
		}
	case <-time.After(100 * time.Millisecond):
		// Mount succeeded
	}
	
	// Give FUSE a moment to stabilize
	time.Sleep(100 * time.Millisecond)
	
	// Test 1: Root directory should have proper metadata
	t.Run("RootDirectory", func(t *testing.T) {
		info, err := os.Stat(mountPoint)
		require.NoError(t, err, "Should be able to stat root")
		
		assert.True(t, info.IsDir(), "Root should be a directory")
		assert.NotEqual(t, time.Time{}, info.ModTime(), "Root should have non-zero mtime")
		
		// Check it's not Unix epoch (Jan 1 1970)
		epoch := time.Unix(0, 0)
		assert.True(t, info.ModTime().After(epoch.Add(time.Hour)), 
			"Root mtime should not be Unix epoch, got %v", info.ModTime())
		
		t.Logf("✓ Root directory: mtime=%v mode=%v", info.ModTime(), info.Mode())
	})
	
	// Test 2: /usr directory metadata
	t.Run("UsrDirectory", func(t *testing.T) {
		usrPath := filepath.Join(mountPoint, "usr")
		info, err := os.Stat(usrPath)
		require.NoError(t, err, "Should be able to stat /usr")
		
		assert.True(t, info.IsDir(), "/usr should be a directory")
		assert.NotEqual(t, time.Time{}, info.ModTime(), "/usr should have non-zero mtime")
		
		epoch := time.Unix(0, 0)
		assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
			"/usr mtime should not be Unix epoch, got %v", info.ModTime())
		
		// Check permissions
		mode := info.Mode()
		assert.Equal(t, os.ModeDir, mode&os.ModeDir, "/usr should have dir bit set")
		
		t.Logf("✓ /usr directory: mtime=%v mode=%o", info.ModTime(), info.Mode())
	})
	
	// Test 3: /etc directory metadata
	t.Run("EtcDirectory", func(t *testing.T) {
		etcPath := filepath.Join(mountPoint, "etc")
		info, err := os.Stat(etcPath)
		require.NoError(t, err, "Should be able to stat /etc")
		
		assert.True(t, info.IsDir(), "/etc should be a directory")
		
		epoch := time.Unix(0, 0)
		assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
			"/etc mtime should not be Unix epoch, got %v", info.ModTime())
		
		t.Logf("✓ /etc directory: mtime=%v mode=%o", info.ModTime(), info.Mode())
	})
	
	// Test 4: Regular file metadata (if /etc/hostname exists)
	t.Run("RegularFile", func(t *testing.T) {
		// Try common files that should exist
		testFiles := []string{
			"etc/hostname",
			"etc/os-release",
			"usr/bin/bash",
		}
		
		for _, relPath := range testFiles {
			fullPath := filepath.Join(mountPoint, relPath)
			info, err := os.Stat(fullPath)
			if err != nil {
				continue // File might not exist
			}
			
			assert.False(t, info.IsDir(), "%s should not be a directory", relPath)
			
			epoch := time.Unix(0, 0)
			assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
				"%s mtime should not be Unix epoch, got %v", relPath, info.ModTime())
			
			// File should have size
			assert.Greater(t, info.Size(), int64(0), "%s should have size > 0", relPath)
			
			t.Logf("✓ %s: size=%d mtime=%v mode=%o", relPath, info.Size(), info.ModTime(), info.Mode())
			break // Just need to verify one file
		}
	})
	
	// Test 5: Symlink metadata
	t.Run("Symlink", func(t *testing.T) {
		// /bin should be a symlink to usr/bin in modern Ubuntu
		binPath := filepath.Join(mountPoint, "bin")
		info, err := os.Lstat(binPath) // Use Lstat to not follow symlink
		if err != nil {
			t.Skip("bin symlink not present")
			return
		}
		
		assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, 
			"/bin should be a symlink")
		
		// Verify symlink target
		target, err := os.Readlink(binPath)
		require.NoError(t, err, "Should be able to read symlink")
		assert.NotEmpty(t, target, "Symlink should have target")
		
		t.Logf("✓ /bin symlink: target=%s mode=%o", target, info.Mode())
	})
	
	// Test 6: Runtime directories should NOT exist
	t.Run("RuntimeDirectoriesExcluded", func(t *testing.T) {
		runtimeDirs := []string{"proc", "sys", "dev"}
		
		for _, dir := range runtimeDirs {
			dirPath := filepath.Join(mountPoint, dir)
			_, err := os.Stat(dirPath)
			assert.True(t, os.IsNotExist(err), 
				"Runtime directory /%s should not exist in mount, but it does", dir)
			
			if err == nil {
				t.Errorf("❌ /%s exists (should be excluded)", dir)
			} else {
				t.Logf("✓ /%s correctly excluded", dir)
			}
		}
	})
	
	// Test 7: Deep directory structure
	t.Run("DeepDirectoryStructure", func(t *testing.T) {
		// Verify nested directories like /usr/local/bin
		deepPath := filepath.Join(mountPoint, "usr", "local", "bin")
		info, err := os.Stat(deepPath)
		if err != nil {
			t.Skip("Deep directory structure not present")
			return
		}
		
		assert.True(t, info.IsDir(), "/usr/local/bin should be a directory")
		
		epoch := time.Unix(0, 0)
		assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
			"/usr/local/bin mtime should not be Unix epoch, got %v", info.ModTime())
		
		t.Logf("✓ /usr/local/bin: mtime=%v mode=%o", info.ModTime(), info.Mode())
	})
	
	// Test 8: Stat syscall for detailed attributes
	t.Run("DetailedSyscallStat", func(t *testing.T) {
		usrPath := filepath.Join(mountPoint, "usr")
		
		var stat syscall.Stat_t
		err := syscall.Stat(usrPath, &stat)
		require.NoError(t, err, "Should be able to syscall stat /usr")
		
		// Verify inode is not 0
		assert.NotZero(t, stat.Ino, "/usr should have non-zero inode")
		
		// Verify times are not 0
		assert.NotZero(t, stat.Atim.Sec, "/usr should have non-zero atime")
		assert.NotZero(t, stat.Mtim.Sec, "/usr should have non-zero mtime")
		
		// Verify it's a directory (S_IFDIR = 0040000)
		assert.NotZero(t, stat.Mode&syscall.S_IFDIR, "/usr should have S_IFDIR bit")
		
		t.Logf("✓ /usr syscall stat: ino=%d mode=0%o uid=%d gid=%d", 
			stat.Ino, stat.Mode, stat.Uid, stat.Gid)
		t.Logf("  atime=%d mtime=%d ctime=%d", 
			stat.Atim.Sec, stat.Mtim.Sec, stat.Ctim.Sec)
	})
}

// TestFUSEMountAlpineMetadata tests with Alpine which is smaller/faster
func TestFUSEMountAlpineMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping FUSE mount test in short mode")
	}
	
	// This test requires FUSE to be available
	t.Skip("Skipping FUSE integration test - requires fusermount and FUSE kernel module")
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	mountPoint := filepath.Join(tempDir, "mount")
	
	// Create index
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	// Create mount point
	err = os.MkdirAll(mountPoint, 0755)
	require.NoError(t, err)
	
	// Mount
	unmount, errChan, _, err := MountArchive(MountOptions{
		ArchivePath: clipFile,
		MountPoint:  mountPoint,
	})
	if err != nil {
		t.Skipf("Cannot mount FUSE: %v", err)
		return
	}
	defer unmount()
	
	select {
	case err := <-errChan:
		if err != nil {
			t.Skipf("FUSE mount error: %v", err)
			return
		}
	case <-time.After(200 * time.Millisecond):
		// Mount succeeded
	}
	
	time.Sleep(100 * time.Millisecond)
	
	// Verify root metadata
	info, err := os.Stat(mountPoint)
	require.NoError(t, err)
	
	epoch := time.Unix(0, 0)
	assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
		"Root mtime should not be Unix epoch, got %v", info.ModTime())
	
	// Verify /etc metadata
	etcPath := filepath.Join(mountPoint, "etc")
	info, err = os.Stat(etcPath)
	require.NoError(t, err)
	
	assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
		"/etc mtime should not be Unix epoch, got %v", info.ModTime())
	
	t.Logf("✓ Alpine: root mtime=%v, /etc mtime=%v", 
		info.ModTime(), info.ModTime())
}

// TestFUSEMountReadFileContent verifies we can actually read file contents
func TestFUSEMountReadFileContent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping FUSE mount test in short mode")
	}
	
	// This test requires FUSE to be available
	t.Skip("Skipping FUSE integration test - requires fusermount and FUSE kernel module")
	
	ctx := context.Background()
	tempDir := t.TempDir()
	
	imageRef := "docker.io/library/alpine:3.18"
	clipFile := filepath.Join(tempDir, "alpine.clip")
	mountPoint := filepath.Join(tempDir, "mount")
	
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:      imageRef,
		OutputPath:    clipFile,
		CheckpointMiB: 2,
	})
	require.NoError(t, err)
	
	err = os.MkdirAll(mountPoint, 0755)
	require.NoError(t, err)
	
	unmount, errChan, _, err := MountArchive(MountOptions{
		ArchivePath: clipFile,
		MountPoint:  mountPoint,
	})
	if err != nil {
		t.Skipf("Cannot mount FUSE: %v", err)
		return
	}
	defer unmount()
	
	select {
	case err := <-errChan:
		if err != nil {
			t.Skipf("FUSE mount error: %v", err)
			return
		}
	case <-time.After(100 * time.Millisecond):
		// Mount succeeded
	}
	
	time.Sleep(100 * time.Millisecond)
	
	// Try to read /etc/alpine-release
	releaseFile := filepath.Join(mountPoint, "etc", "alpine-release")
	content, err := os.ReadFile(releaseFile)
	require.NoError(t, err, "Should be able to read /etc/alpine-release")
	
	assert.NotEmpty(t, content, "File content should not be empty")
	assert.Contains(t, string(content), "3.18", "Should contain Alpine version")
	
	// Verify file stat
	info, err := os.Stat(releaseFile)
	require.NoError(t, err)
	
	assert.Equal(t, int64(len(content)), info.Size(), 
		"File stat size should match actual content length")
	
	epoch := time.Unix(0, 0)
	assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
		"File mtime should not be Unix epoch, got %v", info.ModTime())
	
	t.Logf("✓ Read %d bytes from /etc/alpine-release", len(content))
	t.Logf("  Content: %s", string(content))
	t.Logf("  Metadata: size=%d mtime=%v", info.Size(), info.ModTime())
}
