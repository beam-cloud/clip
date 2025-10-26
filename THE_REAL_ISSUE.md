# The REAL Issue with Beta9 Integration

## 🎯 You're Right - No Wait Should Be Needed

If v1 worked without waiting, v2 should too. The "wandered into deleted directory" error is **NOT** about FUSE mount timing.

## 🔍 The Actual Problem

Looking at your error:
```
error mounting "proc" to rootfs at "/proc": 
finding existing subpath of "proc": 
wandered into deleted directory "/dev/shm/build-f744960d/layer-0/merged/proc"
```

This tells us:
1. runc can't find `/proc` in the merged overlay
2. The path is in `/dev/shm` (tmpfs)
3. The directory appears "deleted" to the kernel

## 💡 Root Cause: You're Calling the Wrong Function

Based on the error pattern, I believe beta9 is:

```go
// ❌ WRONG: Trying to index from a local OCI directory
ociDirPath := filepath.Join(imageTmpDir, baseImage.Repo)  // e.g., "/tmp/ubuntu"
err = c.createIndexOnlyArchive(ctx, ociDirPath, archivePath, baseImage.Tag)
```

**This cannot work.** Clip v2's `CreateFromOCIImage()` expects a **registry reference**, not a filesystem path.

When you pass a path like `/tmp/ubuntu:latest` or `oci:/tmp/ubuntu`, it:
1. Treats it as a registry URL
2. Tries to fetch from `https://tmp/ubuntu` (fails)
3. Falls back to some other behavior
4. Creates a corrupt index with empty symlinks

## ✅ The Fix: Skip Local Copy Entirely

### For v2 Build Flow:

```go
if c.config.ImageService.ClipVersion == 2 {
    // ✅ NO skopeo copy needed!
    // ✅ NO local OCI directory!
    // ✅ Index directly from source registry
    
    err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
        ImageRef:   *request.BuildOptions.SourceImage,  // Use THIS, not a local path
        OutputPath: archivePath,
    })
    
    if err != nil {
        // Fall back to v1
    } else {
        // Upload metadata to S3 and return
        return c.registry.Push(ctx, archivePath, request.ImageId)
    }
}

// v1 fallback: Do the full skopeo copy + unpack + archive
```

## 🗺️ Comparison: v1 vs v2 Workflow

### v1 (Legacy) - Data Extraction
```
Source Registry
    ↓ skopeo copy
Local OCI directory (/tmp/ubuntu/)
    ↓ umoci unpack
Extracted rootfs (/tmp/bundle/rootfs/)
    ↓ clip.CreateArchive()
Clip archive with ALL file data
    ↓ upload to S3
S3 bucket (contains full archive)
```

**Why v1 didn't have issues:** 
- It extracts to a real rootfs directory first
- createArchive() walks the filesystem
- All files and symlinks are concrete

### v2 (New) - Index Only
```
Source Registry (docker.io/library/ubuntu:24.04)
    ↓ clip.CreateFromOCIImage()  ← Read layers via HTTP, no local copy
Clip index (metadata only, 0.3% size)
    ↓ upload to S3
S3 bucket (contains only metadata)

Runtime:
    ↓ clip.MountArchive()
FUSE mount (lazy loads from registry)
    ↓ overlayfs
Container rootfs
```

**Why v2 is different:**
- No local extraction
- Reads tar streams directly from registry
- Must use registry reference, not local path

## 📋 Your Beta9 Code Should Look Like This

```go
func (c *ImageClient) PullAndArchiveImage(ctx context.Context, outputLogger *slog.Logger, request *types.ContainerRequest) error {
	baseImage, err := image.ExtractImageNameAndTag(*request.BuildOptions.SourceImage)
	if err != nil {
		return err
	}

	outputLogger.Info("Inspecting image name and verifying architecture...\n")
	if err := c.inspectAndVerifyImage(ctx, request); err != nil {
		return err
	}

	startTime := time.Now()
	
	// Check clip version
	if c.config.ImageService.ClipVersion == 2 {
		outputLogger.Info("Creating index-only archive (Clip v2)...\n")
		
		archivePath := filepath.Join("/tmp", fmt.Sprintf("%s.clip", request.ImageId))
		defer os.RemoveAll(archivePath)

		// ✅ CRITICAL: Use the source registry reference directly
		// Do NOT use a local OCI directory path!
		err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
			ImageRef:      *request.BuildOptions.SourceImage,
			OutputPath:    archivePath,
			CheckpointMiB: 2,
		})
		
		if err != nil {
			log.Warn().Err(err).Msg("clip v2 failed, falling back to v1")
			outputLogger.Info("Falling back to v1...\n")
			// Fall through to v1 code below
		} else {
			// Success - push to registry
			err = c.registry.Push(ctx, archivePath, request.ImageId)
			if err != nil {
				return fmt.Errorf("failed to push v2 index: %w", err)
			}
			
			elapsed := time.Since(startTime)
			log.Info().Str("image_id", request.ImageId).Dur("duration", elapsed).Msg("v2 completed")
			return nil
		}
	}

	// v1 fallback (original code)
	baseTmpBundlePath := filepath.Join(c.imageBundlePath, baseImage.Repo)
	os.MkdirAll(baseTmpBundlePath, 0755)
	
	// ... rest of v1 code (skopeo copy, unpack, archive) ...
}
```

## ❓ Questions for You

1. **Do you have a `createIndexOnlyArchive()` function?** If so, delete it - it's not needed.
2. **Are you using `/dev/shm` for overlay mounts?** If so, change to `/var/lib/clip`.
3. **What does `*request.BuildOptions.SourceImage` contain?** (e.g., "docker.io/library/ubuntu:24.04")

## ✅ Expected Behavior After Fix

- ✅ No skopeo copy for v2 (faster!)
- ✅ Symlinks work correctly (`/bin -> usr/bin`)
- ✅ /proc directory exists
- ✅ No "deleted directory" errors
- ✅ Container starts successfully
- ✅ **No artificial waits needed**

The key insight: **Clip v2 is fundamentally different from v1. Don't try to make it work with local OCI directories - use registry references directly.**
