# Beta9 Integration - Key Changes for Clip v2

## 🎯 Root Cause of "wandered into deleted directory"

The error is **NOT** about waiting for FUSE to stabilize. It's about:

1. **Wrong function call**: Using a local OCI directory path with Clip v2 APIs
2. **Wrong mount location**: Using `/dev/shm` (tmpfs) for overlay layers
3. **Missing overlay flags**: Not using `index=off,metacopy=off` for FUSE

## ❌ What Beta9 is Currently Doing (WRONG)

```go
// 1. Copy image to local OCI directory
skopeoClient.Copy(sourceImage, "oci:/tmp/ubuntu:latest")

// 2. Try to index the LOCAL directory
createIndexOnlyArchive(ctx, "/tmp/ubuntu", archivePath, "latest")
// ❌ This is wrong! Clip v2 expects a REGISTRY reference, not a local path

// 3. Mount with overlay in /dev/shm
// ❌ tmpfs + overlayfs + FUSE = unstable
```

## ✅ What Beta9 Should Do (CORRECT)

```go
// 1. Skip skopeo entirely for v2!
// Clip v2 reads directly from the source registry

// 2. Index from the registry reference
clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
    ImageRef: "docker.io/library/ubuntu:24.04",  // ✅ Registry ref, not local path!
    OutputPath: archivePath,
})

// 3. Mount with proper paths (not /dev/shm)
mounter := clip.NewOverlayMounter(clip.OverlayMountOptions{
    MountBase:  "/var/lib/clip",  // ✅ Persistent storage
    RootfsBase: "/run/clip",       // ✅ Persistent storage
})
mount, err := mounter.Mount(ctx, clipStorage)
// Use mount.RootfsPath with runc
```

## 🔧 Exact Code Changes Needed

### Change 1: In `PullAndArchiveImage()`

**REMOVE this entire section:**
```go
baseTmpBundlePath := filepath.Join(c.imageBundlePath, baseImage.Repo)
os.MkdirAll(baseTmpBundlePath, 0755)

copyDir := filepath.Join(imageTmpDir, baseImage.Repo)
os.MkdirAll(copyDir, 0755)
defer os.RemoveAll(baseTmpBundlePath)
defer os.RemoveAll(copyDir)

dest := fmt.Sprintf("oci:%s:%s", baseImage.Repo, baseImage.Tag)

// Copy with skopeo
err = c.skopeoClient.Copy(ctx, *request.BuildOptions.SourceImage, dest, ...)

// Try to index local directory
ociDirPath := filepath.Join(imageTmpDir, baseImage.Repo)
err = c.createIndexOnlyArchive(ctx, ociDirPath, archivePath, baseImage.Tag)
```

**REPLACE with:**
```go
// v2: Index directly from source registry (no local copy needed!)
err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
    ImageRef:      *request.BuildOptions.SourceImage,
    OutputPath:    archivePath,
    CheckpointMiB: 2,
})
```

### Change 2: Remove `createIndexOnlyArchive()` function

**DELETE this function entirely** - it's not needed and doesn't work with Clip v2.

### Change 3: Fix Mount Paths

**FIND this pattern:**
```go
layerPath := filepath.Join("/dev/shm", containerID, "layer-0")
```

**REPLACE with:**
```go
// Use persistent storage, not tmpfs (/dev/shm)
layerPath := filepath.Join("/var/lib/clip", containerID, "layer-0")
```

Or better yet, use Clip's overlay mounter which handles all of this.

### Change 4: Fix Overlay Creation

**FIND this pattern:**
```go
syscall.Mount("overlay", mergedPath, "overlay", 0, fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", ...))
```

**REPLACE with:**
```go
// Add flags for FUSE compatibility
opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,index=off,metacopy=off", ...)
syscall.Mount("overlay", mergedPath, "overlay", syscall.MS_NODEV|syscall.MS_NOSUID, opts)
```

## 🎯 Summary of Changes

| Issue | Wrong Approach | Correct Approach |
|-------|----------------|------------------|
| Indexing | Index local OCI dir | Index from registry ref |
| Skopeo | Always copy locally | Skip for v2 |
| Mount location | /dev/shm (tmpfs) | /var/lib/clip or /run/clip |
| Overlay flags | Default | index=off,metacopy=off |
| API | createIndexOnlyArchive() | clip.CreateFromOCIImage() |

## 🧪 How to Verify

After making changes:

```bash
# 1. Index should work
clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
    ImageRef: "docker.io/library/ubuntu:24.04",
    OutputPath: "/tmp/test.clip",
})

# 2. Check index content
go run debug_mount.go /tmp/test.clip
# Should show: ✓ /bin -> usr/bin (not empty)

# 3. Container should start
runc run <container>
# Should work without "deleted directory" error
```

## 💡 Why This Works

1. **No local OCI directory** - Clip v2 reads from registry directly
2. **Proper storage paths** - Kernel can handle overlayfs + FUSE on persistent storage
3. **Correct flags** - `index=off,metacopy=off` tells overlay to be FUSE-friendly
4. **Registry references** - Clip v2 designed for this, not local paths

## 🚀 Expected Results

After fix:
- ✅ Symlinks work: `/bin -> usr/bin` (not empty)
- ✅ /proc directory exists and is accessible
- ✅ No "deleted directory" errors
- ✅ Container starts successfully
- ✅ Faster (skips skopeo copy step)
- ✅ Less disk usage (no local OCI copy needed)
