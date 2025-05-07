package clipv2

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
)

// fileMeta holds metadata for test files.
type fileMeta struct {
	name     string
	size     int
	content  []byte
	checksum string
}

func TestCreateAndExpandArchive_LargeFiles(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "clip-test-input-*")
	if err != nil {
		t.Fatalf("Failed to create temporary input directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	testFilesData := []fileMeta{
		{name: "file1.txt", size: 1024 * 1024},               // 1MB file
		{name: "file2.txt", size: 5 * 1024 * 1024},           // 5MB file
		{name: "subdir/file3.txt", size: 10 * 1024 * 1024},   // 10MB file
		{name: "subdir/file4.txt", size: 30 * 1024 * 1024},   // 30MB file
		{name: "subdir/file5.txt", size: 1024 * 1024 * 1024}, // 1GB file
	}

	// Generate content and calculate checksums
	for i := range testFilesData {
		testFilesData[i].content = generateRandomContent(testFilesData[i].size)
		testFilesData[i].checksum = calculateChecksum(testFilesData[i].content)
	}

	// Create test input files using the helper
	createTestInputFiles(t, tempDir, testFilesData)

	// Create a temporary directory for the archive
	archiveDir, err := os.MkdirTemp("", "clip-archive-*")
	if err != nil {
		t.Fatalf("Failed to create temporary archive directory: %v", err)
	}
	defer os.RemoveAll(archiveDir)

	// Create the archive
	options := CreateOptions{
		IndexID:   "1234567890",
		InputPath: tempDir,
		LocalPath: archiveDir,
		Verbose:   false,
	}

	// Assuming CreateArchive is defined in the same package (e.g., archive.go)
	err = CreateArchive(options)
	if err != nil {
		t.Fatalf("Failed to create archive: %v", err)
	}

	// Verify the archive was created (basic check)
	archiveFilePath := filepath.Join(archiveDir, options.IndexID, "index.clip")
	fileInfo, err := os.Stat(archiveFilePath)
	if err != nil {
		t.Fatalf("Failed to stat archive file %s: %v", archiveFilePath, err)
	}
	if fileInfo.Size() == 0 {
		t.Error("Archive file was created but is empty")
	}

	extractDir, err := os.MkdirTemp("", "clip-extract-*")
	if err != nil {
		t.Fatalf("Failed to create extraction directory: %v", err)
	}
	defer os.RemoveAll(extractDir)

	// Expand the archive
	err = ExpandLocalArchive(context.Background(), ExtractOptions{
		IndexID:    "1234567890",
		SourcePath: archiveDir,
		OutputPath: extractDir,
		Verbose:    true,
	})
	if err != nil {
		t.Fatalf("Failed to extract archive: %v", err)
	}

	// Verify extracted files using the helper
	for _, tf := range testFilesData {
		verifyExtractedFile(t, extractDir, tf)
	}

	// Verify directory structure using the helper
	expectedDirs := []struct {
		name string
		perm os.FileMode
	}{
		{"subdir", 0755},
	}

	for _, ed := range expectedDirs {
		verifyExtractedDirectory(t, extractDir, ed.name, ed.perm)
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

		// Create a temporary directory for the archive
		archiveDir, err := os.MkdirTemp("", "clip-archive-*")
		if err != nil {
			log.Fatal(err)
		}
		defer os.RemoveAll(archiveDir)

		// Create the archive
		options := CreateOptions{
			IndexID:   "1234567890",
			InputPath: tmpDir,
			LocalPath: archiveDir,
			Verbose:   false,
		}

		start := time.Now()
		err = CreateArchive(options)
		if err != nil {
			b.Fatalf("Failed to create archive: %v", err)
		}

		duration := time.Since(start)
		b.Logf("Archive creation took %s\n", duration)
	}
}

// generateRandomContent creates a slice of bytes of a given size with random data.
func generateRandomContent(size int) []byte {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// calculateChecksum computes the SHA256 checksum of a byte slice.
func calculateChecksum(content []byte) string {
	h := sha256.New()
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// createTestInputFiles creates a set of test files in the specified base directory.
func createTestInputFiles(t *testing.T, baseDir string, files []fileMeta) {
	t.Helper()
	for _, tf := range files {
		filePath := filepath.Join(baseDir, tf.name)
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			t.Fatalf("Failed to create directory for %s: %v", tf.name, err)
		}

		if err := os.WriteFile(filePath, tf.content, 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", tf.name, err)
		}
	}
}

// verifyExtractedFile checks the properties of a single extracted file.
func verifyExtractedFile(t *testing.T, extractDir string, expectedFile fileMeta) {
	t.Helper()
	extractedPath := filepath.Join(extractDir, expectedFile.name)

	info, err := os.Stat(extractedPath)
	if err != nil {
		t.Errorf("Failed to stat extracted file %s: %v", expectedFile.name, err)
		return
	}

	if info.Mode().Perm() != 0644 {
		t.Errorf("Incorrect permissions for %s: got %v, want 0644", expectedFile.name, info.Mode().Perm())
	}

	if info.Size() != int64(expectedFile.size) {
		t.Errorf("Incorrect file size for %s: got %d, want %d", expectedFile.name, info.Size(), expectedFile.size)
	}

	file, err := os.Open(extractedPath)
	if err != nil {
		t.Errorf("Failed to open extracted file %s: %v", expectedFile.name, err)
		return
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Errorf("Failed to read extracted file %s for checksum: %v", expectedFile.name, err)
		return
	}

	extractedChecksum := hex.EncodeToString(hash.Sum(nil))
	if extractedChecksum != expectedFile.checksum {
		t.Errorf("Checksum mismatch for %s:\ngot:  %s\nwant: %s", expectedFile.name, extractedChecksum, expectedFile.checksum)
	}
}

// verifyExtractedDirectory checks the properties of a single extracted directory.
func verifyExtractedDirectory(t *testing.T, extractDir string, dirName string, expectedPerm os.FileMode) {
	t.Helper()
	dirPath := filepath.Join(extractDir, dirName)

	info, err := os.Stat(dirPath)
	if err != nil {
		t.Errorf("Failed to stat directory %s: %v", dirName, err)
		return
	}

	if !info.IsDir() {
		t.Errorf("%s is not a directory", dirName)
	}

	if info.Mode().Perm() != expectedPerm {
		t.Errorf("Incorrect permissions for directory %s: got %v, want %v", dirName, info.Mode().Perm(), expectedPerm)
	}
}
