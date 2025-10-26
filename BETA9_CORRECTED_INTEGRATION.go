// CORRECTED Beta9 Integration for Clip v2
// This replaces the PullAndArchiveImage method in your Builder

package image

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/beta9/pkg/types"
	"github.com/beam-cloud/clip/pkg/clip"
	// your other imports...
)

// PullAndArchiveImage - CORRECTED VERSION
func (c *ImageClient) PullAndArchiveImage(ctx context.Context, outputLogger *slog.Logger, request *types.ContainerRequest) error {
	baseImage, err := image.ExtractImageNameAndTag(*request.BuildOptions.SourceImage)
	if err != nil {
		return err
	}

	outputLogger.Info("Inspecting image name and verifying architecture...\n")
	if err := c.inspectAndVerifyImage(ctx, request); err != nil {
		return err
	}

	// For v2: Skip the skopeo copy and local OCI directory entirely!
	if c.config.ImageService.ClipVersion == 2 {
		outputLogger.Info("Creating index-only archive (Clip v2)...\n")
		
		// Use source image reference directly (not a local path!)
		startTime := time.Now()
		archivePath := filepath.Join("/tmp", fmt.Sprintf("%s.%s.tmp", request.ImageId, c.registry.ImageFileExtension))
		defer os.RemoveAll(archivePath)

		// ✅ CRITICAL FIX: Use the source image registry reference directly
		// This reads from the registry, not a local OCI layout directory
		err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
			ImageRef:      *request.BuildOptions.SourceImage, // e.g., "docker.io/library/ubuntu:24.04"
			OutputPath:    archivePath,
			CheckpointMiB: 2,
			Verbose:       false,
		})
		
		if err != nil {
			log.Warn().Err(err).Msg("clip v2 indexing failed, falling back to v1")
			outputLogger.Info("Clip v2 not available, falling back to v1 method...\n")
			// Fall through to v1 path below
		} else {
			// v2 succeeded - push the metadata-only index
			outputLogger.Info("Uploading index to registry...\n")
			err = c.registry.Push(ctx, archivePath, request.ImageId)
			if err != nil {
				log.Error().Str("image_id", request.ImageId).Err(err).Msg("failed to push v2 index")
				return err
			}

			elapsed := time.Since(startTime)
			outputLogger.Info(fmt.Sprintf("✓ Clip v2 index created and uploaded (%.2fs)\n", elapsed.Seconds()))
			log.Info().Str("image_id", request.ImageId).Dur("duration", elapsed).Msg("v2 index completed")
			
			// For v2, we can estimate size from the source
			imageBytes, _ := c.skopeoClient.InspectSizeInBytes(ctx, *request.BuildOptions.SourceImage, request.BuildOptions.SourceImageCreds)
			imageSizeMB := float64(imageBytes) / 1024 / 1024
			metrics.RecordImageCopySpeed(imageSizeMB, elapsed)
			
			return nil
		}
	}

	// v1 (legacy): Full extraction flow with skopeo copy
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

	// v1: unpack and create data-carrying archive
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

// REMOVE the createIndexOnlyArchive function entirely - it's not needed
// Clip v2 reads directly from registries, not local OCI directories
