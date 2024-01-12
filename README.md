# CLIP

CLIP (Compact and Lightweight Immutable Packaging) is a simple file format, similar to tar, but designed for storage of read-only data. The primary feature of CLIP is its transparency, enabling direct access to its content without extraction, even over remote storage. CLIP can be mounted as an S3-backed FUSE filesystem, or can be extracted like a tar file.

It is primary used as the image format for the [Beam](https://github.com/beam-cloud/beam) distributed container engine.

## Features

- **Transparency**: CLIP files are transparent, which means you do not need to extract them to access their content, even over remote storage
- **Mountable**: You can mount a CLIP file and access its content directly using a FUSE filesystem
- **Extractable**: CLIP files can be extracted just like tar files
- **Remote-First**: CLIP is designed with remote storage in mind. It works seamlessly with various cloud storage services (like S3) and can be easily integrated with other object stores.

## Contributing

We welcome contributions! Just submit a PR.

## License

CLIP filesystem is under the Apache license. See the [LICENSE](LICENSE.md) file for more details.

## Support

If you encounter any issues or have feature requests, please open an issue on our [GitHub page](https://github.com/beam-cloud/clip).