package clip

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archive/tar"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/require"
)

func generateRandomContent(size int) []byte {
	content := make([]byte, size)
	rand.Read(content)
	return content
}

func calculateChecksum(content []byte) string {
	hash := sha256.New()
	hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

func TestCreateArchive(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "clip-test-*")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create some test files in the temporary directory with larger sizes
	testFiles := []struct {
		name     string
		size     int
		content  []byte
		checksum string
	}{
		{"file1.txt", 1024 * 1024, nil, ""},             // 1MB file
		{"file2.txt", 5 * 1024 * 1024, nil, ""},         // 5MB file
		{"subdir/file3.txt", 10 * 1024 * 1024, nil, ""}, // 10MB file
	}

	// Generate content and calculate checksums
	for i := range testFiles {
		testFiles[i].content = generateRandomContent(testFiles[i].size)
		testFiles[i].checksum = calculateChecksum(testFiles[i].content)
	}

	for _, tf := range testFiles {
		// Create subdirectories
		filePath := filepath.Join(tempDir, tf.name)
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			t.Fatalf("Failed to create directory for %s: %v", tf.name, err)
		}

		// Create the file
		if err := os.WriteFile(filePath, tf.content, 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", tf.name, err)
		}
	}

	// Create a temporary file for the archive
	archiveFile, err := os.CreateTemp("", "test-archive-*.clip")
	if err != nil {
		t.Fatalf("Failed to create temporary archive file: %v", err)
	}
	archiveFile.Close()
	defer os.Remove(archiveFile.Name())

	// Create the archive
	options := CreateOptions{
		InputPath:  tempDir,
		OutputPath: archiveFile.Name(),
	}

	err = CreateArchive(options)
	if err != nil {
		t.Fatalf("Failed to create archive: %v", err)
	}

	// Verify the archive was created
	fileInfo, err := os.Stat(archiveFile.Name())
	if err != nil {
		t.Fatalf("Failed to stat archive file: %v", err)
	}

	// Check that the archive file exists and has a reasonable size
	if fileInfo.Size() == 0 {
		t.Error("Archive file was created but is empty")
	}

	// Create a temporary directory for extraction
	extractDir, err := os.MkdirTemp("", "clip-extract-*")
	if err != nil {
		t.Fatalf("Failed to create extraction directory: %v", err)
	}
	defer os.RemoveAll(extractDir)

	// Extract the archive
	extractOptions := ExtractOptions{
		InputFile:  archiveFile.Name(),
		OutputPath: extractDir,
	}

	err = ExtractArchive(extractOptions)
	if err != nil {
		t.Fatalf("Failed to extract archive: %v", err)
	}

	// Verify extracted files
	for _, tf := range testFiles {
		extractedPath := filepath.Join(extractDir, tf.name)

		// Check if file exists
		info, err := os.Stat(extractedPath)
		if err != nil {
			t.Errorf("Failed to stat extracted file %s: %v", tf.name, err)
			continue
		}

		// Check file permissions (should be 0644)
		if info.Mode().Perm() != 0644 {
			t.Errorf("Incorrect permissions for %s: got %v, want 0644", tf.name, info.Mode().Perm())
		}

		// Check file size
		if info.Size() != int64(tf.size) {
			t.Errorf("Incorrect file size for %s: got %d, want %d", tf.name, info.Size(), tf.size)
		}

		// Read file and calculate checksum
		file, err := os.Open(extractedPath)
		if err != nil {
			t.Errorf("Failed to open extracted file %s: %v", tf.name, err)
			continue
		}
		defer file.Close()

		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			t.Errorf("Failed to read extracted file %s: %v", tf.name, err)
			continue
		}

		extractedChecksum := hex.EncodeToString(hash.Sum(nil))
		if extractedChecksum != tf.checksum {
			t.Errorf("Checksum mismatch for %s:\ngot: %s\nwant: %s", tf.name, extractedChecksum, tf.checksum)
		}
	}

	// Verify directory structure
	expectedDirs := []string{
		"subdir",
	}

	for _, dir := range expectedDirs {
		dirPath := filepath.Join(extractDir, dir)
		info, err := os.Stat(dirPath)
		if err != nil {
			t.Errorf("Failed to stat directory %s: %v", dir, err)
			continue
		}

		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}

		// Check directory permissions (should be 0755)
		if info.Mode().Perm() != 0755 {
			t.Errorf("Incorrect permissions for directory %s: got %v, want 0755", dir, info.Mode().Perm())
		}
	}
}

func BenchmarkCreateArchiveFromOCIImage(b *testing.B) {
	for i := 0; i < b.N; i++ {
		tmpDir, err := os.MkdirTemp("", "oci-rootfs-*")
		if err != nil {
			b.Fatalf("Failed to create temporary directory for rootfs: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		image := "nginx:latest"
		img, err := crane.Pull(image)
		if err != nil {
			b.Fatalf("Failed to pull OCI image: %v", err)
		}

		f, err := os.Create(filepath.Join(tmpDir, "image.tar"))
		if err != nil {
			b.Fatalf("Failed to create image tar file: %v", err)
		}

		if err := crane.Export(img, f); err != nil {
			b.Fatalf("Failed to export OCI image: %v", err)
		}

		f.Seek(0, io.SeekStart)
		tarReader := tar.NewReader(f)
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatalf("Failed to read tar header: %v", err)
			}

			targetPath := filepath.Join(tmpDir, header.Name)
			switch header.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
					b.Fatalf("Failed to create directory: %v", err)
				}
			case tar.TypeReg:
				outFile, err := os.Create(targetPath)
				if err != nil {
					b.Fatalf("Failed to create file: %v", err)
				}
				if _, err := io.Copy(outFile, tarReader); err != nil {
					outFile.Close()
					b.Fatalf("Failed to write file: %v", err)
				}
				outFile.Close()
			}
		}

		archiveFile, err := os.CreateTemp("", "test-archive-*.clip")
		if err != nil {
			b.Fatalf("Failed to create temporary archive file: %v", err)
		}
		archiveFile.Close()
		defer os.Remove(archiveFile.Name())

		options := CreateOptions{
			InputPath:  tmpDir,
			OutputPath: archiveFile.Name(),
		}

		start := time.Now()
		err = CreateArchive(options)
		if err != nil {
			b.Fatalf("Failed to create archive: %v", err)
		}

		duration := time.Since(start)
		b.Logf("Archive creation took %s", duration)
	}
}

func TestExtractMetadataIndexCompatibility(t *testing.T) {
	nodes := []*common.ClipNode{{Path: "c"}, {Path: "a"}, {Path: "b"}, {Path: "b", ContentHash: "replacement"}}
	var index bytes.Buffer
	require.NoError(t, gob.NewEncoder(&index).Encode(nodes))
	for _, tc := range []struct {
		name   string
		length int64
		valid  bool
	}{{"unordered and duplicate nodes", int64(index.Len()), true}, {"truncated index boundary", int64(index.Len() - 1), false}, {"index exceeds file", int64(index.Len() + 1), false}} {
		t.Run(tc.name, func(t *testing.T) {
			header := common.ClipArchiveHeader{ClipFileFormatVersion: common.ClipFileFormatVersion, IndexPos: common.ClipHeaderLength, IndexLength: tc.length}
			copy(header.StartBytes[:], common.ClipFileStartBytes)
			archiver := NewClipArchiver()
			encoded, err := archiver.EncodeHeader(&header)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "image.clip")
			require.NoError(t, os.WriteFile(path, append(encoded, index.Bytes()...), 0600))
			metadata, err := archiver.ExtractMetadata(path)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 3, metadata.Index.Len())
			require.Equal(t, "replacement", metadata.Index.Get(&common.ClipNode{Path: "b"}).(*common.ClipNode).ContentHash)
		})
	}
}

func TestTranscodeMetadata(t *testing.T) {
	archiver := NewClipArchiver()
	index := archiver.newIndex()
	node := &common.ClipNode{NodeType: common.FileNode, Path: "/file", Target: "target", ContentHash: "\x00\xff", DataPos: -7, DataLen: 42,
		Attr:   fuse.Attr{Ino: 1, Size: 2, Blocks: 3, Atime: 4, Mtime: 5, Ctime: 6, Atimensec: 7, Mtimensec: 8, Ctimensec: 9, Mode: 10, Nlink: 11, Owner: fuse.Owner{Uid: 12, Gid: 13}, Rdev: 14, Blksize: 15, Padding: 16},
		Remote: &common.RemoteRef{LayerDigest: "layer", UOffset: -11, ULength: 17}}
	index.Set(node)
	index.Set(&common.ClipNode{Path: "/nil", NodeType: common.SymLinkNode})
	source := filepath.Join(t.TempDir(), "image.rclip")
	info := common.OCIStorageInfo{Layers: []string{"layer"}, DecompressedHashByLayer: map[string]string{"b": "2", "a": "1"}}
	require.NoError(t, archiver.CreateRemoteArchive(info, &common.ClipArchiveMetadata{Index: index}, source))
	original, err := os.ReadFile(source)
	require.NoError(t, err)
	first := source + ".batch"
	require.NoError(t, archiver.TranscodeMetadata(source, first, nil))
	require.NoError(t, gob.NewEncoder(io.Discard).Encode(struct{ Unrelated string }{"other type"}))
	second := source + ".second"
	require.NoError(t, archiver.TranscodeMetadata(source, second, nil))
	encoded, err := os.ReadFile(first)
	require.NoError(t, err)
	other, err := os.ReadFile(second)
	require.NoError(t, err)
	require.Equal(t, encoded, other)
	metadata, err := archiver.ExtractMetadata(first)
	require.NoError(t, err)
	require.Equal(t, calculateChecksum(original), metadata.OriginalArchiveHash)
	require.Equal(t, int64(len(original)), metadata.OriginalArchiveSize)
	require.Equal(t, index.Len(), metadata.Index.Len())
	require.Equal(t, node, metadata.Index.Get(node))
	require.Nil(t, metadata.Index.Get(&common.ClipNode{Path: "/nil"}).(*common.ClipNode).Remote)
	require.Equal(t, info, metadata.StorageInfo)
	require.Equal(t, original[int64(len(original))-metadata.Header.StorageInfoLength:], encoded[metadata.Header.StorageInfoPos:])
	require.Error(t, archiver.TranscodeMetadata(source, source, nil))
	failed := source + ".directory"
	require.NoError(t, os.Mkdir(failed, 0700))
	require.Error(t, archiver.TranscodeMetadata(source, failed, nil))
	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(source), ".clip-batch-*"))
	require.NoError(t, err)
	require.Empty(t, temporary)
	unchanged, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, original, unchanged)
}

func TestNodeBatchRejectsInvalidInput(t *testing.T) {
	encoded := encodeNodeBatch([]*common.ClipNode{{Path: "node"}})
	for _, invalid := range [][]byte{nil, {0xff}, encoded[:len(encoded)-1], append(encoded, 0)} {
		_, err := decodeNodeBatch(invalid)
		require.Error(t, err)
	}
}

func TestBatchedIndexRejectsOversizedCompressedFrame(t *testing.T) {
	index := make([]byte, 56)
	copy(index, "CLIPBIN2")
	binary.LittleEndian.PutUint64(index[40:], common.ClipHeaderLength)
	binary.LittleEndian.PutUint32(index[48:], 1)
	length := uint32(2*metadataBatchMaxBytes + 1)
	binary.LittleEndian.PutUint32(index[52:], length)
	file, err := os.CreateTemp(t.TempDir(), "sparse-index")
	require.NoError(t, err)
	defer file.Close()
	_, err = file.Write(index)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(int64(len(index))+int64(length)))
	metadata := &common.ClipArchiveMetadata{Header: common.ClipArchiveHeader{IndexLength: int64(len(index)) + int64(length)}, Index: NewClipArchiver().newIndex()}
	require.ErrorContains(t, NewClipArchiver().decodeIndexV2(file, metadata), "invalid v2 index frame length")
}

func TestBatchedCodecGolden(t *testing.T) {
	index := NewClipArchiver().newIndex()
	for i := 0; i < 40000; i++ {
		index.Set(&common.ClipNode{NodeType: common.FileNode, Path: fmt.Sprintf("/usr/lib/library-%03d.so", i), ContentHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Attr: fuse.Attr{Ino: uint64(i + 1), Size: 1048576, Mode: 0100644, Nlink: 1}, Remote: &common.RemoteRef{LayerDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", UOffset: int64(i) * 1048576, ULength: 1048576}})
	}
	encoded, err := NewClipArchiver().encodeIndexV2(index, make([]byte, 32), common.ClipHeaderLength)
	require.NoError(t, err)
	// The batch version must change if codec options or dependency changes alter this hash.
	require.Equal(t, "f9bde62963b666d90e23ca93ed120a374fc513d109024ba5d76a93cc123fcfa4", calculateChecksum(encoded))
}

func TestBatchedIndexSplitsLongPaths(t *testing.T) {
	archiver := NewClipArchiver()
	index := archiver.newIndex()
	for i := 0; i < 18000; i++ {
		index.Load(&common.ClipNode{Path: fmt.Sprintf("/%05d/%s", i, strings.Repeat("p", 3800))})
	}
	encoded, err := archiver.encodeIndexV2(index, make([]byte, 32), common.ClipHeaderLength)
	require.NoError(t, err)
	file, err := os.CreateTemp(t.TempDir(), "index")
	require.NoError(t, err)
	defer file.Close()
	_, err = file.Write(encoded)
	require.NoError(t, err)
	metadata := &common.ClipArchiveMetadata{Header: common.ClipArchiveHeader{IndexLength: int64(len(encoded))}, Index: archiver.newIndex()}
	require.NoError(t, archiver.decodeIndexV2(file, metadata))
	require.Equal(t, index.Len(), metadata.Index.Len())
	require.Equal(t, index.Min(), metadata.Index.Min())
	require.Equal(t, index.Max(), metadata.Index.Max())
}
