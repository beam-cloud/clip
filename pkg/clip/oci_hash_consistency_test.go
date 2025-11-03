package clip

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecompressedHashConsistency verifies that the hash computed during indexing
// matches the hash of the actual decompressed layer data that will be stored
func TestDecompressedHashConsistency(t *testing.T) {
	// Create a test tar archive with some files
	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)

	// Add a file
	file1Content := []byte("This is file 1 content with some data")
	err := tw.WriteHeader(&tar.Header{
		Name: "file1.txt",
		Mode: 0644,
		Size: int64(len(file1Content)),
	})
	require.NoError(t, err)
	_, err = tw.Write(file1Content)
	require.NoError(t, err)

	// Add another file
	file2Content := []byte("File 2 has different content that is longer to test hashing")
	err = tw.WriteHeader(&tar.Header{
		Name: "dir/file2.txt",
		Mode: 0644,
		Size: int64(len(file2Content)),
	})
	require.NoError(t, err)
	_, err = tw.Write(file2Content)
	require.NoError(t, err)

	// Add a directory
	err = tw.WriteHeader(&tar.Header{
		Name:     "emptydir/",
		Mode:     0755,
		Typeflag: tar.TypeDir,
	})
	require.NoError(t, err)

	err = tw.Close()
	require.NoError(t, err)

	// Compress the tar archive with gzip
	compressedBuffer := new(bytes.Buffer)
	gzw := gzip.NewWriter(compressedBuffer)
	_, err = io.Copy(gzw, tarBuffer)
	require.NoError(t, err)
	err = gzw.Close()
	require.NoError(t, err)

	compressedData := compressedBuffer.Bytes()
	t.Logf("Compressed size: %d bytes", len(compressedData))

	// Step 1: Compute hash during indexing (what oci_indexer.go does)
	indexingHash, err := computeHashDuringIndexing(compressedData)
	require.NoError(t, err)
	t.Logf("Hash during indexing: %s", indexingHash)

	// Step 2: Compute hash during decompression (what oci.go does)
	decompressedHash, decompressedData, err := computeHashDuringDecompression(compressedData)
	require.NoError(t, err)
	t.Logf("Hash during decompression: %s", decompressedHash)
	t.Logf("Decompressed size: %d bytes", len(decompressedData))

	// Step 3: Compute hash from the decompressed data (what ContentCache does)
	contentCacheHash := computeContentCacheHash(decompressedData)
	t.Logf("Hash from ContentCache: %s", contentCacheHash)

	// Step 4: Verify all three hashes match
	assert.Equal(t, indexingHash, decompressedHash, 
		"Hash during indexing should match hash during decompression")
	assert.Equal(t, decompressedHash, contentCacheHash,
		"Hash during decompression should match ContentCache hash")
	assert.Equal(t, indexingHash, contentCacheHash,
		"Hash during indexing should match ContentCache hash")
}

// computeHashDuringIndexing simulates how we compute the hash during indexing in oci_indexer.go
func computeHashDuringIndexing(compressedData []byte) (string, error) {
	// Create gzip reader
	gzr, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", err
	}
	defer gzr.Close()

	// Hash the decompressed data as we read it (this is what oci_indexer.go does)
	hasher := sha256.New()
	hashingReader := io.TeeReader(gzr, hasher)

	// Create tar reader to consume the stream (simulating the indexing process)
	tr := tar.NewReader(hashingReader)
	
	// Read through all tar entries (like the indexer does)
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		// Consume file content (the indexer skips over file contents)
		_, err = io.Copy(io.Discard, tr)
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// computeHashDuringDecompression simulates how we compute the hash during decompression in oci.go
func computeHashDuringDecompression(compressedData []byte) (string, []byte, error) {
	// Create gzip reader
	gzr, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", nil, err
	}
	defer gzr.Close()

	// Hash the decompressed data as we write it (this is what oci.go does)
	hasher := sha256.New()
	buffer := new(bytes.Buffer)
	multiWriter := io.MultiWriter(buffer, hasher)
	
	_, err = io.Copy(multiWriter, gzr)
	if err != nil {
		return "", nil, err
	}

	return hex.EncodeToString(hasher.Sum(nil)), buffer.Bytes(), nil
}

// computeContentCacheHash simulates how the ContentCache computes the hash
func computeContentCacheHash(data []byte) string {
	hashBytes := sha256.Sum256(data)
	return hex.EncodeToString(hashBytes[:])
}

// TestRealLayerHashConsistency tests with the actual indexing code to ensure consistency
func TestRealLayerHashConsistency(t *testing.T) {
	// Create test data
	testData := []byte("This is test data for real layer testing with multiple iterations")
	
	// Create a tar archive
	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)
	
	// Add multiple files to make it realistic
	for i := 0; i < 5; i++ {
		content := append(testData, byte(i))
		filename := "testfile_" + string(rune('0'+i)) + ".txt"
		
		err := tw.WriteHeader(&tar.Header{
			Name: filename,
			Mode: 0644,
			Size: int64(len(content)),
		})
		require.NoError(t, err)
		_, err = tw.Write(content)
		require.NoError(t, err)
	}
	
	err := tw.Close()
	require.NoError(t, err)

	// Compress with gzip
	compressedBuffer := new(bytes.Buffer)
	gzw := gzip.NewWriter(compressedBuffer)
	_, err = io.Copy(gzw, tarBuffer)
	require.NoError(t, err)
	err = gzw.Close()
	require.NoError(t, err)

	compressedData := compressedBuffer.Bytes()

	// Test using the actual indexing code
	archiver := &ClipArchiver{}
	index := archiver.newIndex()
	
	gzipIndex, indexedHash, err := archiver.indexLayerOptimized(
		context.Background(),
		io.NopCloser(bytes.NewReader(compressedData)),
		"sha256:test123",
		index,
		IndexOCIImageOptions{CheckpointMiB: 2},
	)
	require.NoError(t, err)
	require.NotNil(t, gzipIndex)
	t.Logf("Indexed hash: %s", indexedHash)

	// Now decompress and verify
	_, decompressedData, err := computeHashDuringDecompression(compressedData)
	require.NoError(t, err)

	// Compute what ContentCache would compute
	contentCacheHash := computeContentCacheHash(decompressedData)
	t.Logf("ContentCache hash: %s", contentCacheHash)

	// The critical assertion
	assert.Equal(t, indexedHash, contentCacheHash,
		"Hash from indexing must match what ContentCache computes from decompressed data")
}

// TestHashWithDiskFile verifies the hash remains consistent when writing to disk
func TestHashWithDiskFile(t *testing.T) {
	// Create test content
	testContent := []byte("Test content for disk file hashing verification")
	
	// Create tar
	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)
	err := tw.WriteHeader(&tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: int64(len(testContent)),
	})
	require.NoError(t, err)
	_, err = tw.Write(testContent)
	require.NoError(t, err)
	err = tw.Close()
	require.NoError(t, err)

	// Compress
	compressedBuffer := new(bytes.Buffer)
	gzw := gzip.NewWriter(compressedBuffer)
	_, err = io.Copy(gzw, tarBuffer)
	require.NoError(t, err)
	err = gzw.Close()
	require.NoError(t, err)

	compressedData := compressedBuffer.Bytes()

	// Compute hash during indexing
	indexedHash, err := computeHashDuringIndexing(compressedData)
	require.NoError(t, err)

	// Write to disk like oci.go does, using the indexed hash as the filename
	tmpDir := t.TempDir()
	diskPath := tmpDir + "/" + indexedHash  // Name file with the indexed hash (like production does)
	
	gzr, err := gzip.NewReader(bytes.NewReader(compressedData))
	require.NoError(t, err)
	defer gzr.Close()
	
	tmpFile, err := os.Create(diskPath)
	require.NoError(t, err)
	
	hasher := sha256.New()
	multiWriter := io.MultiWriter(tmpFile, hasher)
	_, err = io.Copy(multiWriter, gzr)
	tmpFile.Close()
	require.NoError(t, err)
	
	diskWriteHash := hex.EncodeToString(hasher.Sum(nil))

	// Read back from disk and compute hash (simulating sha256sum command)
	diskData, err := os.ReadFile(diskPath)
	require.NoError(t, err)
	
	diskReadHash := computeContentCacheHash(diskData)
	
	t.Logf("File named: %s", indexedHash)
	t.Logf("SHA256 of file contents: %s", diskReadHash)

	// All hashes must match
	assert.Equal(t, indexedHash, diskWriteHash, "Indexed hash must match hash computed while writing to disk")
	assert.Equal(t, diskWriteHash, diskReadHash, "Hash while writing to disk must match hash when reading from disk")
	assert.Equal(t, indexedHash, diskReadHash, "CRITICAL: Indexed hash must match hash of data on disk (filename must match content hash)")
	
	// This is the key assertion that mirrors the user's finding:
	// The file is named with indexedHash, and sha256sum of that file should equal indexedHash
	assert.Equal(t, indexedHash, diskReadHash, 
		"File named '%s' should have SHA256 hash '%s' but has '%s'",
		indexedHash, indexedHash, diskReadHash)
}
