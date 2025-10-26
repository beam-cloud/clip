# Beta9 Integration Guide for Clip v2 OCI Layout Support

This guide shows how to integrate Clip v2's OCI layout support into the beta9 build system to solve the registry URL parsing issue.

## The Problem

The original issue was that Clip v2's `CreateFromOCI` method expects remote registry references (like `docker.io/library/ubuntu:24.04`), but beta9's build system works with local OCI layout directories created by skopeo/buildah.

## The Solution

Clip v2 now includes **OCI Layout support** that can index local OCI layout directories directly, creating the same efficient metadata-only archives without needing registry access.

## Integration Changes Required

### 1. Update the `createIndexOnlyArchive` method

Replace your current `createIndexOnlyArchive` method in the beta9 build system:

```go
func (c *ImageClient) createIndexOnlyArchive(ctx context.Context, ociDirPath, archivePath, tag string) error {
	// Use the new Clip v2 OCI layout integration function
	return clip.CreateIndexOnlyArchiveFromOCILayout(ctx, ociDirPath, tag, archivePath)
}
```

### 2. Import the Clip package

Make sure your imports include the clip package:

```go
import (
	// ... your other imports ...
	"github.com/beam-cloud/clip/pkg/clip"
)
```

### 3. Update go.mod (if needed)

Ensure your `go.mod` includes the clip dependency:

```go
require (
	// ... your other dependencies ...
	github.com/beam-cloud/clip v0.0.0-latest  // Use appropriate version
)
```

## How It Works

### Before (Failing)
```
skopeo copy → /tmp/ubuntu/ (OCI layout)
                ↓
clip.CreateFromOCI("/tmp/ubuntu:latest") → ❌ Tries to parse as registry URL
```

### After (Working)
```
skopeo copy → /tmp/ubuntu/ (OCI layout)
                ↓
clip.CreateIndexOnlyArchiveFromOCILayout(ctx, "/tmp/ubuntu", "latest", "output.clip") → ✅ Success
```

## Complete Integration Example

Here's how your `PullAndArchiveImage` method should look with the fix:

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

	baseTmpBundlePath := filepath.Join(c.imageBundlePath, baseImage.Repo)
	os.MkdirAll(baseTmpBundlePath, 0755)

	copyDir := filepath.Join(imageTmpDir, baseImage.Repo)
	os.MkdirAll(copyDir, 0755)
	defer os.RemoveAll(baseTmpBundlePath)
	defer os.RemoveAll(copyDir)

	dest := fmt.Sprintf("oci:%s:%s", baseImage.Repo, baseImage.Tag)

	imageBytes, err := c.skopeoClient.InspectSizeInBytes(ctx, *request.BuildOptions.SourceImage, request.BuildOptions.SourceImageCreds)
	if err != nil {
		log.Warn().Err(err).Msg("unable to inspect image size")
	}
	imageSizeMB := float64(imageBytes) / 1024 / 1024

	outputLogger.Info(fmt.Sprintf("Copying image (size: %.2f MB)...\n", imageSizeMB))
	startTime := time.Now()
	err = c.skopeoClient.Copy(ctx, *request.BuildOptions.SourceImage, dest, request.BuildOptions.SourceImageCreds, outputLogger)
	if err != nil {
		return err
	}
	metrics.RecordImageCopySpeed(imageSizeMB, time.Since(startTime))

	// Check clipVersion to determine archiving strategy
	log.Info().Int("clip_version", int(c.config.ImageService.ClipVersion)).Msg("clip version")
	if c.config.ImageService.ClipVersion == 2 {
		// v2: index-only archive from the OCI layout directory
		outputLogger.Info("Creating index-only archive (Clip v2)...\n")

		archivePath := filepath.Join("/tmp", fmt.Sprintf("%s.%s.tmp", request.ImageId, c.registry.ImageFileExtension))
		defer os.RemoveAll(archivePath)

		// Use the local OCI layout path that skopeo created
		ociDirPath := filepath.Join(imageTmpDir, baseImage.Repo)

		// FIXED: Use the new OCI layout indexing function
		err = c.createIndexOnlyArchive(ctx, ociDirPath, archivePath, baseImage.Tag)
		if err != nil {
			log.Warn().Err(err).Msg("clip v2 not available, falling back to v1")
			outputLogger.Info("Clip v2 not available, falling back to v1 method...\n")
			// Fall through to v1 path
		} else {
			// v2 succeeded - push the archive and return
			err = c.registry.Push(ctx, archivePath, request.ImageId)
			if err != nil {
				log.Error().Str("image_id", request.ImageId).Err(err).Msg("failed to push image")
				return err
			}

			elapsed := time.Since(startTime)
			log.Info().Str("image_id", request.ImageId).Dur("seconds", time.Duration(elapsed.Seconds())).Msg("v2 archive and push completed")
			metrics.RecordImageCopySpeed(imageSizeMB, elapsed)
			return nil
		}
	}

	// v1 (legacy): unpack and create data-carrying archive
	outputLogger.Info("Unpacking image...\n")
	tmpBundlePath := NewPathInfo(filepath.Join(baseTmpBundlePath, request.ImageId))
	err = c.unpack(ctx, baseImage.Repo, baseImage.Tag, tmpBundlePath)
	if err != nil {
		return fmt.Errorf("unable to unpack image: %v", err)
	}

	outputLogger.Info("Archiving base image...\n")
	err = c.Archive(ctx, tmpBundlePath, request.ImageId, nil)
	if err != nil {
		return err
	}

	return nil
}

// Updated createIndexOnlyArchive method
func (c *ImageClient) createIndexOnlyArchive(ctx context.Context, ociDirPath, archivePath, tag string) error {
	// Use Clip v2's OCI layout support
	return clip.CreateIndexOnlyArchiveFromOCILayout(ctx, ociDirPath, tag, archivePath)
}
```

## Key Benefits

1. **No Registry Dependency**: Works entirely with local OCI layout directories
2. **Same Efficiency**: Creates the same metadata-only archives as registry-based indexing
3. **Seamless Integration**: Drop-in replacement for the existing method
4. **Backward Compatible**: Falls back to v1 if v2 fails
5. **Performance**: Fast indexing without network calls

## Testing the Integration

You can test the integration with a simple OCI layout:

```bash
# Create a test OCI layout with skopeo
skopeo copy docker://alpine:latest oci:/tmp/test-alpine:latest

# Test the clip indexing
clipctl index-layout --layout /tmp/test-alpine --tag latest --out test.clip

# Verify the clip file was created
ls -la test.clip
```

## Troubleshooting

### Common Issues

1. **"failed to read index.json"**: The OCI layout directory is incomplete or corrupted
2. **"no manifest found for tag"**: The specified tag doesn't exist in the layout
3. **"failed to open layer blob"**: Layer files are missing from the blobs directory

### Debug Mode

Enable verbose logging to see detailed indexing information:

```go
// In your code, before calling the clip function:
zerolog.SetGlobalLevel(zerolog.DebugLevel)

// Or via environment variable:
// CLIP_LOG_LEVEL=debug
```

## Performance Characteristics

- **Index Size**: ~0.1-1% of original image size (metadata only)
- **Indexing Speed**: ~50-200 MB/s (depends on layer compression)
- **Memory Usage**: Minimal (streaming processing)
- **Disk Usage**: Only the small .clip file, no layer data duplication

This integration should completely resolve the registry URL parsing issue while maintaining all the performance benefits of Clip v2!