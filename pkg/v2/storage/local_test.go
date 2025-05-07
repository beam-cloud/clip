package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	common "github.com/beam-cloud/clip/pkg/common"
	clipv2 "github.com/beam-cloud/clip/pkg/v2"
	// log "github.com/rs/zerolog/log" // Not strictly needed for test assertions
)

// Helper to create a temporary directory and populate it with chunk files
func createTempChunkDirWithFiles(t *testing.T, chunkFiles map[string][]byte) (chunkDirPath string, cleanupFunc func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "local_storage_test_chunks_*")
	if err != nil {
		t.Fatalf("Failed to create temp chunk dir: %v", err)
	}

	for hash, content := range chunkFiles {
		// Ensure chunk files are named according to how LocalClipStorage expects to find them
		chunkFilePath := filepath.Join(tempDir, hash+clipv2.ChunkSuffix)
		err := os.WriteFile(chunkFilePath, content, 0644)
		if err != nil {
			os.RemoveAll(tempDir) // cleanup partially created
			t.Fatalf("Failed to write temp chunk file %s: %v", chunkFilePath, err)
		}
	}

	return tempDir, func() { os.RemoveAll(tempDir) }
}

// Helper to create a dummy archive file (metadata file path is checked by NewLocalClipStorage)
func createDummyArchiveFile(t *testing.T) (archivePath string, cleanupFunc func()) {
	t.Helper()
	tempFile, err := os.CreateTemp("", "dummy_archive_*.clip")
	if err != nil {
		t.Fatalf("Failed to create dummy archive file: %v", err)
	}
	archivePath = tempFile.Name()
	if err := tempFile.Close(); err != nil {
		os.Remove(archivePath) // Attempt cleanup
		t.Fatalf("Failed to close dummy archive file: %v", err)
	}
	return archivePath, func() { os.Remove(archivePath) }
}

func TestLocalClipStorage_ReadFile_Scenarios(t *testing.T) {
	// Helper to generate mock data
	generateByteSlice := func(size int, startValue byte) []byte {
		data := make([]byte, size)
		for i := 0; i < size; i++ {
			data[i] = byte(i) + startValue
		}
		return data
	}

	// Common data for tests
	const baseChunkSize = 50
	chunk0 := "chunk0local"
	chunk1 := "chunk1local"

	// Content for chunk files
	// chunk0Content: 0-49
	// chunk1Content: 50-99
	chunk0Content := generateByteSlice(baseChunkSize, 0)
	chunk1Content := generateByteSlice(baseChunkSize, baseChunkSize) // Values 50-99

	testCases := []struct {
		name                      string
		metadataChunkSizeToUse    int64
		metadataChunkHashesToUse  []string
		chunkFilesToCreate        map[string][]byte
		nodeDataPos               int64
		nodeDataLen               int64
		readFileOffset            int64
		destBufSize               int
		expectedBytesRead         int
		expectedDestContent       []byte
		expectError               bool
		expectedErrorMsgSubstring string
		expectedPanicMsgSubstring string
	}{
		{
			name:                      "input validation: not a file node",
			metadataChunkSizeToUse:    baseChunkSize,
			nodeDataPos:               0,
			nodeDataLen:               10,
			destBufSize:               10,
			expectError:               true,
			expectedErrorMsgSubstring: "cannot ReadFile on non-file node type",
		},
		{
			name:                      "input validation: negative readFileOffset",
			metadataChunkSizeToUse:    baseChunkSize,
			nodeDataPos:               0,
			nodeDataLen:               10,
			readFileOffset:            -1,
			destBufSize:               10,
			expectError:               true,
			expectedErrorMsgSubstring: "negative offset -1 is invalid",
		},
		{
			name:                   "input validation: zero length dest buffer",
			metadataChunkSizeToUse: baseChunkSize,
			nodeDataPos:            0,
			nodeDataLen:            10,
			destBufSize:            0,
			expectedBytesRead:      0,
			expectError:            false,
		},
		{
			name:                     "single chunk: partial read from start",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:              0,
			nodeDataLen:              20,
			destBufSize:              20,
			expectedBytesRead:        20,
			expectedDestContent:      chunk0Content[0:20],
		},
		{
			name:                     "single chunk: partial read from middle",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:              10,
			nodeDataLen:              15,
			destBufSize:              15,
			expectedBytesRead:        15,
			expectedDestContent:      chunk0Content[10:25],
		},
		{
			name:                     "single chunk: read full chunk (node matches chunk size)",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:              0,
			nodeDataLen:              baseChunkSize,
			destBufSize:              baseChunkSize,
			expectedBytesRead:        baseChunkSize,
			expectedDestContent:      chunk0Content,
		},
		{
			name:                     "single chunk: read from middle to end of chunk",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:              30,
			nodeDataLen:              20,
			destBufSize:              20,
			expectedBytesRead:        20,
			expectedDestContent:      chunk0Content[30:50],
		},
		{
			name:                     "multi-chunk: span two chunks (start chunk0, end chunk1)",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0, chunk1},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content, chunk1: chunk1Content},
			nodeDataPos:              30,
			nodeDataLen:              40,
			destBufSize:              40,
			expectedBytesRead:        40,
			expectedDestContent:      append(chunk0Content[30:50], chunk1Content[0:20]...),
		},
		{
			name:                     "multi-chunk: read exactly two full chunks",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0, chunk1},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content, chunk1: chunk1Content},
			nodeDataPos:              0,
			nodeDataLen:              baseChunkSize * 2,
			destBufSize:              baseChunkSize * 2,
			expectedBytesRead:        baseChunkSize * 2,
			expectedDestContent:      append(chunk0Content, chunk1Content...),
		},
		{
			name:                      "edge: node.DataLen is 0, non-empty dest",
			metadataChunkSizeToUse:    baseChunkSize,
			metadataChunkHashesToUse:  []string{chunk0},
			nodeDataPos:               10,
			nodeDataLen:               0,
			destBufSize:               5,
			expectedBytesRead:         0,
			expectedDestContent:       make([]byte, 0),
			expectError:               true,
			expectedErrorMsgSubstring: "destination buffer size 5 is larger than node data length 0",
		},
		{
			name:                     "edge: readFileOffset > 0",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:              5,
			nodeDataLen:              10,
			readFileOffset:           3,
			destBufSize:              10,
			expectedBytesRead:        10,
			expectedDestContent:      chunk0Content[8:18],
		},
		{
			name:                      "edge: read beyond metadata chunks (index out of range for s.metadata.Chunks)",
			metadataChunkSizeToUse:    baseChunkSize,
			metadataChunkHashesToUse:  []string{chunk0},
			chunkFilesToCreate:        map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:               0,
			nodeDataLen:               baseChunkSize + 10,
			destBufSize:               int(baseChunkSize + 10),
			expectError:               true,
			expectedErrorMsgSubstring: "invalid chunk indices for 1 chunks: startChunk 0, endChunk ",
		},
		{
			name:                     "edge: dest buffer smaller than node.DataLen",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content},
			nodeDataPos:              0,
			nodeDataLen:              20,
			destBufSize:              10,
			expectedBytesRead:        10,
			expectedDestContent:      chunk0Content[0:10],
			expectError:              false,
		},
		{
			name:                     "short read: chunk file is smaller than expected read from it",
			metadataChunkSizeToUse:   baseChunkSize,
			metadataChunkHashesToUse: []string{chunk0},
			chunkFilesToCreate:       map[string][]byte{chunk0: chunk0Content[0:30]},
			nodeDataPos:              0,
			nodeDataLen:              40,
			destBufSize:              40,
			expectedBytesRead:        30,
			expectedDestContent:      chunk0Content[0:30],
			expectError:              false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dummyArchivePath, archiveCleanup := createDummyArchiveFile(t)
			defer archiveCleanup()

			chunkDir, dirCleanup := createTempChunkDirWithFiles(t, tc.chunkFilesToCreate)
			defer dirCleanup()

			metadata := &clipv2.ClipV2Archive{
				Header: clipv2.ClipV2ArchiveHeader{
					ChunkSize: tc.metadataChunkSizeToUse,
				},
				Chunks: tc.metadataChunkHashesToUse,
			}

			opts := LocalClipStorageOpts{
				ArchivePath: dummyArchivePath,
				ChunkDir:    chunkDir,
			}
			s, err := NewLocalClipStorage(metadata, opts)
			if err != nil {
				t.Fatalf("NewLocalClipStorage failed: %v", err)
			}

			node := &common.ClipNode{
				NodeType: common.FileNode,
				DataPos:  tc.nodeDataPos,
				DataLen:  tc.nodeDataLen,
				Path:     "test/node/path",
			}
			if tc.name == "input validation: not a file node" {
				node.NodeType = common.DirNode
			}

			dest := make([]byte, tc.destBufSize)

			n, err := s.ReadFile(node, dest, tc.readFileOffset)

			if tc.expectError {
				if err == nil {
					t.Errorf("Expected an error, but got nil")
				} else if tc.expectedErrorMsgSubstring != "" && !strings.Contains(err.Error(), tc.expectedErrorMsgSubstring) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tc.expectedErrorMsgSubstring, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("ReadFile failed unexpectedly: %v", err)
				}
			}

			if !tc.expectError || (tc.expectError && tc.expectedBytesRead > 0) {
				if n != tc.expectedBytesRead {
					t.Errorf("Expected to read %d bytes, got %d", tc.expectedBytesRead, n)
				}

				var relevantDestSlice []byte
				if tc.expectedBytesRead > 0 && tc.expectedBytesRead <= len(dest) {
					relevantDestSlice = dest[:tc.expectedBytesRead]
				} else if tc.expectedBytesRead == 0 {
					relevantDestSlice = make([]byte, 0)
				} else {
					relevantDestSlice = dest
				}

				if len(tc.expectedDestContent) > 0 || tc.expectedBytesRead > 0 {
					if !reflect.DeepEqual(relevantDestSlice, tc.expectedDestContent) {
						t.Errorf("Destination buffer content mismatch.\nGot: %v\nExp: %v\nFull Dest: %v", relevantDestSlice, tc.expectedDestContent, dest)
					}
				}
			}
		})
	}
}
