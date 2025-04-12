# CLIP

CLIP is an image file format designed for lazy-loading images. This works by indexing the underlying RootFS, allowing direct access to an images content without extraction, even over remote storage. These archives (.clip/.rclip files) are then mounted via FUSE, and that path can be provided to container runtimes like runc or docker.

It is used primarily as the image format for the [Beam](https://github.com/beam-cloud/beam) container engine.

## Features

- **Transparency**: CLIP files are transparent, which means you do not need to extract them to access their content, even over remote storage
- **Mountable**: You can mount a CLIP file and access its content directly using a FUSE filesystem
- **Extractable**: CLIP files can be extracted just like tar files.
- **Remote-First**: CLIP is designed with remote storage in mind. It works with any s3 compatible object storage.

## Contributing

We welcome contributions! Just submit a PR.

## License

CLIP filesystem is under the MIT license. See the [LICENSE](LICENSE.md) file for more details.

## Support

If you encounter any issues or have feature requests, please open an issue on our [GitHub page](https://github.com/beam-cloud/clip).
