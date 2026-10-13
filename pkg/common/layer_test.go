package common

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestDecompressLayer(t *testing.T) {
	content := bytes.Repeat([]byte("layer content "), 1000)

	var gzipped bytes.Buffer
	gzw := gzip.NewWriter(&gzipped)
	_, err := gzw.Write(content)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())

	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	zstdCompressed := encoder.EncodeAll(content, nil)
	// zstd:chunked layers are several frames, the last a skippable one.
	chunked := append(encoder.EncodeAll(content[:5000], nil), encoder.EncodeAll(content[5000:], nil)...)
	chunked = append(chunked, 0x50, 0x2a, 0x4d, 0x18, 4, 0, 0, 0, 't', 'o', 'c', '!')

	for name, blob := range map[string][]byte{
		"gzip":         gzipped.Bytes(),
		"zstd":         zstdCompressed,
		"zstd chunked": chunked,
		"uncompressed": content,
	} {
		t.Run(name, func(t *testing.T) {
			reader, err := DecompressLayer(bytes.NewReader(blob))
			require.NoError(t, err)
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, content, got)
		})
	}

	t.Run("empty", func(t *testing.T) {
		reader, err := DecompressLayer(bytes.NewReader(nil))
		require.NoError(t, err)
		got, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Empty(t, got)
	})
}
