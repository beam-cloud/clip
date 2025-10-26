# Beta9 Integration Guide - Clip v2

## The "wandered into deleted directory" Error

This error happens when overlayfs can't access the lower FUSE layer. It's caused by:

1. **FUSE mount not stable** before overlay is created
2. **Race condition** between mount and overlay creation
3. **Using `/dev/shm` for overlay** (tmpfs can be problematic with overlayfs over FUSE)

## ✅ Fix for Beta9

### Option 1: Use Clip's Built-in Overlay (Recommended)

Instead of managing your own overlay mounts, use Clip's overlay orchestration:

```go
import "github.com/beam-cloud/clip/pkg/clip"

// Create and mount with overlay in one step
mounter := clip.NewOverlayMounter(clip.OverlayMountOptions{
    ContainerID:        containerID,
    ImageDigest:        imageID,
    MountBase:          "/var/lib/clip",      // NOT /dev/shm!
    RootfsBase:         "/run/clip",           // NOT /dev/shm!
    UseKernelOverlayfs: true,
})

// This creates:
// - FUSE mount at /var/lib/clip/mnts/<image>/ro
// - Upper layer at /var/lib/clip/upper/<cid>
// - Work dir at /var/lib/clip/work/<cid>
// - Final rootfs at /run/clip/<cid>/rootfs
mount, err := mounter.Mount(ctx, clipStorage)
if err != nil {
    return err
}

// Use mount.RootfsPath for runc
config.Root.Path = mount.RootfsPath
```

### Option 2: Fix Your Existing Overlay Code

If you must use your own overlay system:

```go
// 1. Mount the clip archive
startServer, _, server, err := clip.MountArchive(clip.MountOptions{
    ArchivePath: clipFile,
    MountPoint:  roMountPoint,  // Use persistent storage, NOT /dev/shm
})
if err != nil {
    return err
}

err = startServer()
if err != nil {
    return err
}

// 2. CRITICAL: Wait for FUSE to stabilize
time.Sleep(500 * time.Millisecond)

// 3. Verify mount is accessible
entries, err := os.ReadDir(roMountPoint)
if err != nil {
    return fmt.Errorf("FUSE mount not ready: %w", err)
}
log.Printf("FUSE mount ready with %d entries", len(entries))

// 4. Create overlay with proper flags
// IMPORTANT: Use index=off,metacopy=off for FUSE compatibility
overlayOpts := fmt.Sprintf(
    "lowerdir=%s,upperdir=%s,workdir=%s,index=off,metacopy=off",
    roMountPoint,
    upperDir,  // Must NOT be in /dev/shm
    workDir,   // Must NOT be in /dev/shm
)

err = syscall.Mount("overlay", mergedPath, "overlay", 0, overlayOpts)
if err != nil {
    return fmt.Errorf("overlay mount failed: %w", err)
}

// 5. Verify overlay is accessible
if _, err := os.Stat(filepath.Join(mergedPath, ".")); err != nil {
    return fmt.Errorf("overlay mount not accessible: %w", err)
}
```

## 🚫 Common Mistakes

### ❌ Using /dev/shm for overlay layers

```go
// DON'T DO THIS:
workDir := "/dev/shm/build-xyz/layer-0/work"    // ❌ Causes issues
upperDir := "/dev/shm/build-xyz/layer-0/upper"  // ❌ Causes issues
```

**Why it fails:** `/dev/shm` is tmpfs (RAM-based). Overlayfs over FUSE with tmpfs workdir is unreliable on many kernels.

### ✅ Use persistent storage

```go
// DO THIS:
workDir := "/var/lib/clip/work/build-xyz"      // ✅ Works
upperDir := "/var/lib/clip/upper/build-xyz"    // ✅ Works
```

### ❌ Not waiting for FUSE

```go
// DON'T DO THIS:
startServer()
// Immediately create overlay ❌ RACE CONDITION!
syscall.Mount("overlay", ...)
```

### ✅ Wait and verify

```go
// DO THIS:
startServer()
time.Sleep(500 * time.Millisecond)  // Let FUSE stabilize
os.ReadDir(roMountPoint)            // Verify accessible
syscall.Mount("overlay", ...)       // Now safe
```

### ❌ Missing overlay flags

```go
// DON'T DO THIS:
opts := "lowerdir=...,upperdir=...,workdir=..."  // ❌ Missing flags
```

### ✅ Use FUSE-compatible flags

```go
// DO THIS:
opts := "lowerdir=...,upperdir=...,workdir=...,index=off,metacopy=off"  // ✅
```

## 📋 Recommended Beta9 Changes

### Current Flow (Problematic)
```
1. CreateFromOCIImage() → index file
2. MountArchive() → FUSE at /images/mnt/...
3. Beta9 creates overlay at /dev/shm/... ❌
4. runc fails with "deleted directory"
```

### Fixed Flow (Recommended)
```
1. CreateFromOCIImage() → index file
2. Use Clip's OverlayMounter:
   - Creates FUSE mount at /var/lib/clip/mnts/...
   - Creates overlay at /run/clip/<cid>/rootfs
3. Pass rootfs path to runc ✅
4. Works perfectly
```

### Code Example for Beta9

```go
func (c *ContainerRuntime) setupRootfs(imageID, containerID string) (string, error) {
    // Load the clip archive
    clipPath := fmt.Sprintf("/var/lib/clip/archives/%s.clip", imageID)
    
    metadata, err := clip.NewClipArchiver().ExtractMetadata(clipPath)
    if err != nil {
        return "", err
    }
    
    clipStorage, err := storage.NewClipStorage(storage.ClipStorageOpts{
        ArchivePath: clipPath,
        Metadata:    metadata,
        // ... other opts
    })
    if err != nil {
        return "", err
    }
    
    // Use Clip's overlay mounter
    mounter := clip.NewOverlayMounter(clip.OverlayMountOptions{
        ContainerID:        containerID,
        ImageDigest:        imageID,
        MountBase:          "/var/lib/clip",  // Persistent storage
        RootfsBase:         "/run/clip",       // Persistent storage
        UseKernelOverlayfs: true,
    })
    
    mount, err := mounter.Mount(context.Background(), clipStorage)
    if err != nil {
        return "", err
    }
    
    // Return the rootfs path for runc
    return mount.RootfsPath, nil
    // e.g., "/run/clip/build-xyz/rootfs"
}
```

## 🔍 Debugging

### Check FUSE mount is accessible

```bash
# Should list files, not error
ls /var/lib/clip/mnts/<image>/ro

# Should show bin -> usr/bin (not empty)
ls -la /var/lib/clip/mnts/<image>/ro/bin
```

### Check overlay mount

```bash
# Should show overlay mount
mount | grep overlay

# Should list files
ls /run/clip/<cid>/rootfs

# Symlinks should be correct
ls -la /run/clip/<cid>/rootfs/bin
```

### Check for stale mounts

```bash
# Find stale FUSE mounts
mount | grep fuse

# Unmount if needed
fusermount -u /path/to/mount
```

## 📊 Performance Tips

1. **Use kernel overlayfs** when possible (faster than fuse-overlayfs)
2. **Don't use /dev/shm** for overlay layers
3. **Reuse FUSE mounts** across containers with same image
4. **Enable content cache** for production (dramatically faster)

## ✅ Verification

After implementing the fixes, you should see:

```bash
# FUSE mount works
$ ls /var/lib/clip/mnts/<image>/ro
bin  boot  dev  etc  home  lib  ...

# Symlinks are correct
$ ls -la /var/lib/clip/mnts/<image>/ro/bin
lrwxrwxrwx ... bin -> usr/bin  # ✅ Not empty!

# Overlay works
$ ls /run/clip/<cid>/rootfs
bin  boot  dev  etc  home  lib  ...

# Container starts
$ runc run <cid>
# Success! ✅
```

## 🆘 Still Having Issues?

If you still see "wandered into deleted directory":

1. **Check kernel version**: `uname -r` (need 4.18+ for stable overlayfs+FUSE)
2. **Try fuse-overlayfs**: Set `UseKernelOverlayfs: false`
3. **Check mount namespace**: Ensure runc and FUSE are in same namespace
4. **Increase stabilization delay**: Try `time.Sleep(1 * time.Second)`
5. **Check logs**: Look for "FUSE mount not accessible" errors

## 📝 Summary

**Root cause:** FUSE mount not stable before overlay creation, plus /dev/shm usage.

**Fix:** 
1. Use persistent storage (not /dev/shm)
2. Wait for FUSE to stabilize
3. Use proper overlay flags (index=off,metacopy=off)
4. Or use Clip's built-in OverlayMounter

**Result:** Stable overlayfs over FUSE, no "deleted directory" errors.
