# CLIP

CLIP is a lazy-loading filesystem for container images that enables efficient, on-demand access to OCI images without data duplication. CLIP v2 introduces native OCI image support with registry-backed FUSE filesystems and overlay mounts for container runtimes.

It is used primarily as the image format for the [Beam](https://github.com/beam-cloud/beam) container engine.

## Features

### CLIP v2 (OCI Native)
- **Zero Data Duplication**: Only small metadata indexes stored locally, image data stays in registry
- **OCI Registry Support**: Direct integration with any OCI-compliant registry (Docker Hub, etc.)
- **Container Runtime Ready**: Produces standard rootfs paths for runc/gVisor via overlay mounts
- **Lazy Loading**: Files loaded on-demand via HTTP Range requests with gzip decompression
- **Performance Monitoring**: Built-in metrics for Range GET, decompression, and cache performance

### CLIP v1 (Legacy)
- **Transparency**: CLIP files are transparent, allowing direct access without extraction
- **Mountable**: Mount CLIP files directly using FUSE filesystem
- **Extractable**: CLIP files can be extracted like tar files
- **Remote-First**: Works with S3-compatible object storage

## Quick Start

### Install

```bash
git clone https://github.com/beam-cloud/clip
cd clip
make clipctl
sudo make install-clipctl
```

### OCI Image Usage (v2)

```bash
# Index an OCI image (creates metadata-only .clip file)
clipctl index --image docker.io/library/alpine:latest --out alpine.clip

# Mount image for container use
clipctl mount --image docker.io/library/alpine:latest --cid mycontainer
# Output: /run/clip/mycontainer/rootfs

# Use with runc/gVisor
runc run --bundle /path/to/bundle mycontainer

# Cleanup
clipctl umount --cid mycontainer

# Monitor performance
clipctl metrics --format json
clipctl metrics --serve --port 8080  # HTTP metrics server
```

### Legacy Usage (v1)

```bash
# Create CLIP archive from directory
clipctl create --input /path/to/rootfs --output image.clip

# Mount CLIP file
clipctl mount --archive image.clip --mount-point /mnt/clip

# Extract CLIP file
clipctl extract --archive image.clip --output /path/to/extract
```

## Architecture

CLIP v2 implements a three-layer architecture:

1. **Indexer**: Analyzes OCI images and creates metadata-only indexes
2. **FUSE Layer**: Provides read-only filesystem with lazy loading from registry
3. **Overlay Layer**: Combines read-only FUSE with writable overlay for containers

```
OCI Registry → Indexer → .clip (metadata) → FUSE (RO) → Overlay → Container Rootfs
```

See [CLIP_V2_IMPLEMENTATION.md](CLIP_V2_IMPLEMENTATION.md) for detailed technical documentation.

## Beta9 Integration

For beta9 build system integration with local OCI layouts (buildah/skopeo workflows), see [BETA9_INTEGRATION.md](BETA9_INTEGRATION.md).

## Contributing

We welcome contributions! Just submit a PR.

## License

CLIP filesystem is under the MIT license. See the [LICENSE](LICENSE.md) file for more details.

## Support

If you encounter any issues or have feature requests, please open an issue on our [GitHub page](https://github.com/beam-cloud/clip).
