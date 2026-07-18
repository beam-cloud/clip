package clip

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

func TestIndexOCIImageFromLocalLayout(t *testing.T) {
	compressed := buildLayer(t, []tarEntry{{
		name:     "app/model.txt",
		typeflag: tar.TypeReg,
		content:  "local layout",
	}})
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	})
	require.NoError(t, err)
	layerDigest, err := layer.Digest()
	require.NoError(t, err)
	cache := newFakeBlobContentCache()
	require.Equal(t, strings.TrimPrefix(layerDigest.String(), "sha256:"), cache.put(compressed))

	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	digest, err := img.Digest()
	require.NoError(t, err)

	layoutDir := filepath.Join(t.TempDir(), "image")
	layoutPath, err := layout.Write(layoutDir, empty.Index)
	require.NoError(t, err)
	require.NoError(t, layoutPath.AppendImage(img,
		layout.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
		layout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": "latest"}),
	))

	progress := make(chan OCIIndexProgress, 16)
	storageRef := "registry.example.com/beam/image:test"
	archiver := NewClipArchiver()
	index, layers, _, _, registry, repository, reference, metadata, err := archiver.IndexOCIImage(
		context.Background(),
		IndexOCIImageOptions{
			ImageRef:        storageRef,
			LocalLayoutPath: layoutDir,
			CheckpointMiB:   2,
			Platform:        &v1.Platform{OS: "linux", Architecture: "amd64"},
			ProgressChan:    progress,
			ContentCache:    cache,
			ContentCacheDir: t.TempDir(),
		},
	)
	require.NoError(t, err)
	close(progress)

	require.Greater(t, index.Len(), 1)
	require.Len(t, layers, 1)
	require.Equal(t, "registry.example.com", registry)
	require.Equal(t, "beam/image", repository)
	require.Equal(t, digest.String(), reference)
	require.NotNil(t, metadata)
	require.Equal(t, storageRef, metadata.Name)

	var completed OCIIndexProgress
	for update := range progress {
		if update.Stage == "completed" {
			completed = update
		}
	}
	require.Equal(t, LayerSourceLocalLayout, completed.Source)

	outputPath := filepath.Join(t.TempDir(), "image.clip")
	require.NoError(t, CreateFromOCIImage(context.Background(), CreateFromOCIImageOptions{
		ImageRef:        storageRef,
		LocalLayoutPath: layoutDir,
		OutputPath:      outputPath,
		CheckpointMiB:   2,
		Platform:        &v1.Platform{OS: "linux", Architecture: "amd64"},
	}))
	archive, err := archiver.ExtractMetadata(outputPath)
	require.NoError(t, err)
	info, ok := archive.StorageInfo.(common.OCIStorageInfo)
	if !ok {
		info = *archive.StorageInfo.(*common.OCIStorageInfo)
	}
	require.Equal(t, "registry.example.com", info.RegistryURL)
	require.Equal(t, "beam/image", info.Repository)
	require.Equal(t, digest.String(), info.Reference)

	// Reuse immutable metadata during preparation and mount instead of decoding
	// the full index again.
	require.NoError(t, os.Truncate(outputPath, 0))
	archiveStorage, err := openArchiveStorage(MountOptions{
		ArchivePath: outputPath,
		CachePath:   t.TempDir(),
		Metadata:    archive,
	})
	require.NoError(t, err)
	require.Same(t, archive, archiveStorage.Metadata())
	require.NoError(t, archiveStorage.Cleanup())
}
