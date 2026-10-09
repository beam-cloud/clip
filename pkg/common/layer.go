package common

import (
	"bufio"
	"bytes"
	"errors"
	"io"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// DecompressLayer returns the tar stream of a layer blob, which is gzip, zstd or
// uncompressed. The blob's magic number decides: a layer fetched by digest does
// not know the media type its manifest gave it.
func DecompressLayer(blob io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(blob)
	head, err := buffered.Peek(len(zstdMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	switch {
	case bytes.HasPrefix(head, gzipMagic):
		return gzip.NewReader(buffered)
	case bytes.HasPrefix(head, zstdMagic):
		decoder, err := zstd.NewReader(buffered, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			return nil, err
		}
		return decoder.IOReadCloser(), nil
	default:
		return io.NopCloser(buffered), nil
	}
}
