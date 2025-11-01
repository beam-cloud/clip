# Embedded Image Metadata in OCI Indexes

## Overview

This feature embeds comprehensive OCI image metadata directly into the `.clip` index files, eliminating the need for runtime metadata lookups via tools like `skopeo`. This is particularly beneficial for container runtimes that need image configuration details at startup.

## What's Included

The following metadata is now automatically extracted and embedded during OCI image indexing:

### Image Identification
- **Name**: Full image reference (e.g., `docker.io/library/alpine:3.18`)
- **Digest**: Image manifest digest

### Platform Information
- **Architecture**: Target CPU architecture (e.g., `amd64`, `arm64`)
- **Os**: Operating system (e.g., `linux`)
- **Variant**: Platform variant (optional)

### Build Information
- **Created**: Image creation timestamp
- **DockerVersion**: Docker version used to build (if available)
- **Author**: Image author (optional)

### Runtime Configuration
- **Env**: Environment variables
- **Cmd**: Default command
- **Entrypoint**: Entrypoint configuration
- **User**: Default user
- **WorkingDir**: Default working directory
- **ExposedPorts**: Exposed ports
- **Volumes**: Declared volumes
- **Labels**: Image labels
- **StopSignal**: Stop signal

### Layer Information
- **Layers**: Array of layer digests
- **LayersData**: Detailed per-layer metadata including:
  - MIME type
  - Digest
  - Size
  - Annotations

## Usage

### Creating an OCI Index with Metadata

```go
archiver := clip.NewClipArchiver()
err := archiver.CreateFromOCI(ctx, clip.IndexOCIImageOptions{
    ImageRef:      "docker.io/library/alpine:3.18",
    CheckpointMiB: 2,
}, "alpine.clip")
```

The metadata is automatically extracted and embedded during indexing.

### Accessing Embedded Metadata

```go
// Load the clip file
metadata, err := archiver.ExtractMetadata("alpine.clip")
if err != nil {
    return err
}

// Access OCI storage info
ociInfo, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
if !ok {
    return fmt.Errorf("not an OCI archive")
}

// Access embedded image metadata
if ociInfo.ImageMetadata != nil {
    fmt.Printf("Architecture: %s\n", ociInfo.ImageMetadata.Architecture)
    fmt.Printf("OS: %s\n", ociInfo.ImageMetadata.Os)
    fmt.Printf("Created: %s\n", ociInfo.ImageMetadata.Created)
    fmt.Printf("Env vars: %v\n", ociInfo.ImageMetadata.Env)
    // ... etc
}
```

## Benefits for Beta9

With this feature, Beta9 can:

1. **Eliminate runtime lookups**: No need to call `skopeo inspect` at container startup
2. **Faster container starts**: Metadata is instantly available from the clip file
3. **Reduced dependencies**: No need for skopeo or network access to inspect images
4. **Consistent metadata**: Guaranteed to match the exact image version being used

## Compatibility

The metadata format is designed to be compatible with Beta9's existing `ImageMetadata` struct, ensuring a smooth integration. All fields that Beta9 currently retrieves via skopeo are now available directly from the OCI index.

## Storage Overhead

The embedded metadata adds minimal overhead to the clip file:
- Typical metadata size: 1-5 KB
- Total clip file size remains well under 1 MB for most images (metadata-only)

## Implementation Details

- Metadata extraction is performed during the `IndexOCIImage` operation
- Uses the `go-containerregistry` library to access image config and manifest
- Metadata is serialized using Go's `encoding/gob` format
- Failed metadata extraction does not block index creation (graceful degradation)
