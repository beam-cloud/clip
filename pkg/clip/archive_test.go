package clip

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"archive/tar"

	"github.com/google/go-containerregistry/pkg/crane"
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
		Verbose:    true,
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
		Verbose:    true,
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
			Verbose:    true,
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
