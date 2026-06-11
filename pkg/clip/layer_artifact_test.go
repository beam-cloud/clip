package clip

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tarEntry struct {
	name     string
	typeflag byte
	content  string
	linkname string
}

func buildLayer(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     0644,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Mode = 0755
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typeflag == tar.TypeReg && len(e.content) > 0 {
			_, err := tw.Write([]byte(e.content))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())

	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	_, err := io.Copy(gzw, &tarBuf)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())

	return gzBuf.Bytes()
}

func indexLayerHelper(t *testing.T, archiver *ClipArchiver, compressed []byte, digest string) *LayerArtifact {
	t.Helper()
	artifact, err := archiver.indexLayerToArtifact(
		context.Background(),
		io.NopCloser(bytes.NewReader(compressed)),
		digest,
		IndexOCIImageOptions{CheckpointMiB: 2},
	)
	require.NoError(t, err)
	return artifact
}

func indexPaths(index interface {
	Ascend(pivot interface{}, iter func(item interface{}) bool)
	Min() interface{}
}) map[string]*common.ClipNode {
	nodes := map[string]*common.ClipNode{}
	index.Ascend(index.Min(), func(a interface{}) bool {
		n := a.(*common.ClipNode)
		nodes[n.Path] = n
		return true
	})
	return nodes
}

func TestLayerArtifactRoundTripDeterminism(t *testing.T) {
	archiver := NewClipArchiver()

	layer := buildLayer(t, []tarEntry{
		{name: "dir/", typeflag: tar.TypeDir},
		{name: "dir/a.txt", typeflag: tar.TypeReg, content: "hello"},
		{name: "dir/b.txt", typeflag: tar.TypeReg, content: "world"},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "dir/a.txt"},
		{name: "hard", typeflag: tar.TypeLink, linkname: "dir/a.txt"},
	})

	artifact := indexLayerHelper(t, archiver, layer, "sha256:layer1")

	encoded1, err := EncodeLayerArtifact(artifact)
	require.NoError(t, err)
	encoded2, err := EncodeLayerArtifact(artifact)
	require.NoError(t, err)
	assert.Equal(t, encoded1, encoded2, "artifact encoding must be deterministic")

	decoded, err := DecodeLayerArtifact(encoded1, "sha256:layer1", 2)
	require.NoError(t, err)
	assert.Equal(t, artifact.DecompressedHash, decoded.DecompressedHash)
	assert.Equal(t, artifact.UncompressedSize, decoded.UncompressedSize)
	assert.Equal(t, len(artifact.Entries), len(decoded.Entries))
	assert.Equal(t, artifact.Checkpoints, decoded.Checkpoints)

	// Applying the original and the decoded artifact must produce identical indexes
	idx1 := archiver.newIndex()
	archiver.applyLayerArtifact(idx1, artifact)
	idx2 := archiver.newIndex()
	archiver.applyLayerArtifact(idx2, decoded)

	bytes1, err := archiver.EncodeIndex(idx1)
	require.NoError(t, err)
	bytes2, err := archiver.EncodeIndex(idx2)
	require.NoError(t, err)
	assert.Equal(t, bytes1, bytes2, "replayed index must be byte-identical")

	// Validate index content
	nodes := indexPaths(idx1)
	require.Contains(t, nodes, "/dir/a.txt")
	require.Contains(t, nodes, "/hard")
	assert.Equal(t, nodes["/dir/a.txt"].Attr.Ino, nodes["/hard"].Attr.Ino, "hardlink copies target attrs")
	assert.Equal(t, common.SymLinkNode, nodes["/link"].NodeType)
}

func TestLayerArtifactDecodeValidation(t *testing.T) {
	archiver := NewClipArchiver()
	layer := buildLayer(t, []tarEntry{{name: "f.txt", typeflag: tar.TypeReg, content: "x"}})
	artifact := indexLayerHelper(t, archiver, layer, "sha256:abc")

	encoded, err := EncodeLayerArtifact(artifact)
	require.NoError(t, err)

	_, err = DecodeLayerArtifact(encoded, "sha256:other", 2)
	assert.Error(t, err, "digest mismatch must be rejected")

	_, err = DecodeLayerArtifact(encoded, "sha256:abc", 4)
	assert.Error(t, err, "checkpoint interval mismatch must be rejected")

	_, err = DecodeLayerArtifact([]byte("garbage"), "sha256:abc", 2)
	assert.Error(t, err, "corrupt data must be rejected")
}

func TestLayerArtifactWhiteoutSemantics(t *testing.T) {
	archiver := NewClipArchiver()

	lower := buildLayer(t, []tarEntry{
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/old.conf", typeflag: tar.TypeReg, content: "old"},
		{name: "opt/", typeflag: tar.TypeDir},
		{name: "opt/keep.txt", typeflag: tar.TypeReg, content: "keep"},
		{name: "opt/sub/", typeflag: tar.TypeDir},
		{name: "opt/sub/x.txt", typeflag: tar.TypeReg, content: "x"},
	})
	upper := buildLayer(t, []tarEntry{
		// Whiteout removes /etc/old.conf
		{name: "etc/.wh.old.conf", typeflag: tar.TypeReg},
		// Opaque whiteout clears /opt then re-adds one file
		{name: "opt/.wh..wh..opq", typeflag: tar.TypeReg},
		{name: "opt/new.txt", typeflag: tar.TypeReg, content: "new"},
	})

	lowerArt := indexLayerHelper(t, archiver, lower, "sha256:lower")
	upperArt := indexLayerHelper(t, archiver, upper, "sha256:upper")

	index := archiver.newIndex()
	archiver.applyLayerArtifact(index, lowerArt)
	archiver.applyLayerArtifact(index, upperArt)

	nodes := indexPaths(index)
	assert.NotContains(t, nodes, "/etc/old.conf", "whiteout must remove lower-layer file")
	assert.Contains(t, nodes, "/etc")
	assert.NotContains(t, nodes, "/opt/keep.txt", "opaque whiteout must clear lower-layer dir contents")
	assert.NotContains(t, nodes, "/opt/sub/x.txt")
	assert.NotContains(t, nodes, "/opt/sub")
	assert.Contains(t, nodes, "/opt/new.txt", "same-layer entries after opaque whiteout survive")
	assert.Contains(t, nodes, "/opt")
}
