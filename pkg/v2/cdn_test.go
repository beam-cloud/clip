package clipv2

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jarcoal/httpmock"

	common "github.com/beam-cloud/clip/pkg/common"
)

func TestCDNClipStorage_ReadFile_InputValidation(t *testing.T) {
	s := NewCDNClipStorage(&ClipV2Archive{}, CDNClipStorageOpts{imageID: "testimg", cdnURL: "http://mockcdn"})

	t.Run("NotFileNode", func(t *testing.T) {
		node := &common.ClipNode{NodeType: common.DirNode} // Not a FileNode
		dest := make([]byte, 10)
		n, err := s.ReadFile(node, dest, 0)

		if n != 0 {
			t.Errorf("Expected 0 bytes read, got %d", n)
		}
		if err == nil {
			t.Errorf("Expected error for non-file node, got nil")
		} else {
			expectedErrMsg := fmt.Sprintf("cannot ReadFile on non-file node type: %s", common.DirNode)
			if err.Error() != expectedErrMsg {
				t.Errorf("Expected error message '%s', got '%s'", expectedErrMsg, err.Error())
			}
		}
	})

	t.Run("NegativeOffset", func(t *testing.T) {
		node := &common.ClipNode{NodeType: common.FileNode}
		dest := make([]byte, 10)
		n, err := s.ReadFile(node, dest, -1) // Negative offset

		if n != 0 {
			t.Errorf("Expected 0 bytes read, got %d", n)
		}
		if err == nil {
			t.Errorf("Expected error for negative offset, got nil")
		} else {
			expectedErrMsg := "negative offset -1 is invalid"
			if err.Error() != expectedErrMsg {
				t.Errorf("Expected error message '%s', got '%s'", expectedErrMsg, err.Error())
			}
		}
	})

	t.Run("ZeroLengthDest", func(t *testing.T) {
		node := &common.ClipNode{NodeType: common.FileNode}
		dest := make([]byte, 0) // Zero length destination buffer
		n, err := s.ReadFile(node, dest, 0)

		if n != 0 {
			t.Errorf("Expected 0 bytes read for zero length dest, got %d", n)
		}
		if err != nil {
			t.Errorf("Expected no error for zero length dest, got %v", err)
		}
	})
}

func TestCDNClipStorage_ReadFile_Scenarios(t *testing.T) {
	cdnBaseURL := "http://mockcdn.internal"

	// Helper to generate mock data
	generateMockData := func(size int, startValue byte) []byte {
		data := make([]byte, size)
		for i := 0; i < size; i++ {
			data[i] = byte(i) + startValue
		}
		return data
	}

	// Common data for single-chunk tests
	const singleChunkImageID = "testimage001"
	const singleChunkName = "chunk0hashabcdef"
	singleChunkContentSize := int64(100)
	fullSingleChunkContent := generateMockData(int(singleChunkContentSize), 0) // Bytes 0-99

	// Common data for multi-chunk tests
	const multiChunkImageID = "testimagemulti"
	const multiChunkName0 = "chunk0hashmulti"
	const multiChunkName1 = "chunk1hashmulti"
	multiChunkSegmentSize := int64(50) // Each chunk file contains 50 bytes
	mockDataMultiChunk0 := generateMockData(int(multiChunkSegmentSize), 0)
	mockDataMultiChunk1 := generateMockData(int(multiChunkSegmentSize), byte(multiChunkSegmentSize))

	testCases := []struct {
		name                     string
		imageIDToUse             string
		metadataChunkSizeToUse   int64
		metadataChunkHashesToUse []string
		nodeDataPos              int64
		nodeDataLen              int64
		readFileOffset           int64
		destBufSize              int
		expectedHTTPInteractions []struct {
			chunkNameToUse             string // Just the hash
			expectedRangeHeader        string
			mockResponseData           []byte
			responseStatus             int
			responseContentRangeHeader string // e.g., "bytes 0-49/100"
		}
		expectedBytesRead         int
		expectedDestContent       []byte
		expectError               bool
		expectedErrorMsgSubstring string
	}{
		{
			name:                     "partial read from start of chunk",
			imageIDToUse:             singleChunkImageID,
			metadataChunkSizeToUse:   singleChunkContentSize,
			metadataChunkHashesToUse: []string{singleChunkName},
			nodeDataPos:              0,
			nodeDataLen:              50,
			readFileOffset:           0,
			destBufSize:              50,
			expectedHTTPInteractions: []struct {
				chunkNameToUse             string
				expectedRangeHeader        string
				mockResponseData           []byte
				responseStatus             int
				responseContentRangeHeader string
			}{
				{
					chunkNameToUse:             singleChunkName,
					expectedRangeHeader:        "bytes=0-49",
					mockResponseData:           fullSingleChunkContent[0:50],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 0-49/%d", singleChunkContentSize),
				},
			},
			expectedBytesRead:   50,
			expectedDestContent: fullSingleChunkContent[0:50],
		},
		{
			name:                     "partial read from middle of chunk",
			imageIDToUse:             singleChunkImageID,
			metadataChunkSizeToUse:   singleChunkContentSize,
			metadataChunkHashesToUse: []string{singleChunkName},
			nodeDataPos:              20,
			nodeDataLen:              30,
			readFileOffset:           0,
			destBufSize:              30,
			expectedHTTPInteractions: []struct {
				chunkNameToUse             string
				expectedRangeHeader        string
				mockResponseData           []byte
				responseStatus             int
				responseContentRangeHeader string
			}{
				{
					chunkNameToUse:             singleChunkName,
					expectedRangeHeader:        "bytes=20-49",
					mockResponseData:           fullSingleChunkContent[20:50],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 20-49/%d", singleChunkContentSize),
				},
			},
			expectedBytesRead:   30,
			expectedDestContent: fullSingleChunkContent[20:50],
		},
		{
			name:                     "read until end of node data within chunk",
			imageIDToUse:             singleChunkImageID,
			metadataChunkSizeToUse:   singleChunkContentSize,
			metadataChunkHashesToUse: []string{singleChunkName},
			nodeDataPos:              70,
			nodeDataLen:              30,
			readFileOffset:           0,
			destBufSize:              30,
			expectedHTTPInteractions: []struct {
				chunkNameToUse             string
				expectedRangeHeader        string
				mockResponseData           []byte
				responseStatus             int
				responseContentRangeHeader string
			}{
				{
					chunkNameToUse:             singleChunkName,
					expectedRangeHeader:        "bytes=70-99",
					mockResponseData:           fullSingleChunkContent[70:100],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 70-99/%d", singleChunkContentSize),
				},
			},
			expectedBytesRead:   30,
			expectedDestContent: fullSingleChunkContent[70:100],
		},
		{
			name:                     "read entire node data matching chunk size",
			imageIDToUse:             singleChunkImageID,
			metadataChunkSizeToUse:   singleChunkContentSize,
			metadataChunkHashesToUse: []string{singleChunkName},
			nodeDataPos:              0,
			nodeDataLen:              singleChunkContentSize,
			readFileOffset:           0,
			destBufSize:              int(singleChunkContentSize),
			expectedHTTPInteractions: []struct {
				chunkNameToUse             string
				expectedRangeHeader        string
				mockResponseData           []byte
				responseStatus             int
				responseContentRangeHeader string
			}{
				{
					chunkNameToUse:             singleChunkName,
					expectedRangeHeader:        fmt.Sprintf("bytes=0-%d", singleChunkContentSize-1),
					mockResponseData:           fullSingleChunkContent[0:int(singleChunkContentSize)],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 0-%d/%d", singleChunkContentSize-1, singleChunkContentSize),
				},
			},
			expectedBytesRead:   int(singleChunkContentSize),
			expectedDestContent: fullSingleChunkContent[0:int(singleChunkContentSize)],
		},
		{
			name:                      "node DataLen mismatch with dest size",
			imageIDToUse:              singleChunkImageID,
			metadataChunkSizeToUse:    singleChunkContentSize,
			metadataChunkHashesToUse:  []string{singleChunkName},
			nodeDataPos:               0,
			nodeDataLen:               2,
			readFileOffset:            0,
			destBufSize:               10,
			expectedBytesRead:         0,
			expectError:               true,
			expectedErrorMsgSubstring: "destination buffer size 10 is larger than node data length 2",
		},
		{
			name:                     "partial read with non-zero ReadFile offset",
			imageIDToUse:             singleChunkImageID,
			metadataChunkSizeToUse:   singleChunkContentSize,
			metadataChunkHashesToUse: []string{singleChunkName},
			nodeDataPos:              10,
			nodeDataLen:              30,
			readFileOffset:           5,
			destBufSize:              30,
			expectedHTTPInteractions: []struct {
				chunkNameToUse             string
				expectedRangeHeader        string
				mockResponseData           []byte
				responseStatus             int
				responseContentRangeHeader string
			}{
				{
					chunkNameToUse:             singleChunkName,
					expectedRangeHeader:        "bytes=15-44",
					mockResponseData:           fullSingleChunkContent[15:45],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 15-44/%d", singleChunkContentSize),
				},
			},
			expectedBytesRead:   30,
			expectedDestContent: fullSingleChunkContent[15:45],
		},
		{
			name:                      "request data beyond single available chunk",
			imageIDToUse:              singleChunkImageID,
			metadataChunkSizeToUse:    singleChunkContentSize,
			metadataChunkHashesToUse:  []string{singleChunkName}, // Only one chunk in metadata
			nodeDataPos:               0,
			nodeDataLen:               singleChunkContentSize + 1, // Ask for more than the single chunk
			readFileOffset:            0,
			destBufSize:               int(singleChunkContentSize + 1),
			expectedHTTPInteractions:  nil, // No HTTP calls expected before panic
			expectedBytesRead:         0,
			expectError:               true,
			expectedErrorMsgSubstring: "invalid chunk indices for 1 chunks: startChunk 0, endChunk 2",
		},
		{
			name:                     "multi-chunk read hitting destOffset logic",
			imageIDToUse:             multiChunkImageID,
			metadataChunkSizeToUse:   multiChunkSegmentSize,
			metadataChunkHashesToUse: []string{multiChunkName0, multiChunkName1},
			nodeDataPos:              30,
			nodeDataLen:              40,
			readFileOffset:           0,
			destBufSize:              40,
			expectedHTTPInteractions: []struct {
				chunkNameToUse             string
				expectedRangeHeader        string
				mockResponseData           []byte
				responseStatus             int
				responseContentRangeHeader string
			}{
				{
					chunkNameToUse:             multiChunkName0,
					expectedRangeHeader:        "bytes=30-49",
					mockResponseData:           mockDataMultiChunk0[30:50],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 30-49/%d", multiChunkSegmentSize),
				},
				{
					chunkNameToUse:             multiChunkName1,
					expectedRangeHeader:        "bytes=0-19",
					mockResponseData:           mockDataMultiChunk1[0:20],
					responseStatus:             http.StatusPartialContent,
					responseContentRangeHeader: fmt.Sprintf("bytes 0-19/%d", multiChunkSegmentSize),
				},
			},
			expectedBytesRead:   40,
			expectedDestContent: append(mockDataMultiChunk0[30:50], mockDataMultiChunk1[0:20]...),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Use a new httpmock. τότε ActivateNonDefault for each subtest
			// CDNClipStorage creates its own client, so we mock on that.
			mockClient := &http.Client{}
			httpmock.ActivateNonDefault(mockClient)
			defer httpmock.DeactivateAndReset()

			metadata := &ClipV2Archive{
				Header: ClipV2ArchiveHeader{
					ChunkSize: tc.metadataChunkSizeToUse,
				},
				Chunks: tc.metadataChunkHashesToUse,
			}

			s := NewCDNClipStorage(metadata, CDNClipStorageOpts{cdnURL: cdnBaseURL, imageID: tc.imageIDToUse})
			s.client = mockClient

			node := &common.ClipNode{
				NodeType: common.FileNode,
				DataPos:  tc.nodeDataPos,
				DataLen:  tc.nodeDataLen,
			}
			dest := make([]byte, tc.destBufSize)

			for _, interaction := range tc.expectedHTTPInteractions {
				expectedChunkURL := fmt.Sprintf("%s/%s/chunks/%s", cdnBaseURL, tc.imageIDToUse, interaction.chunkNameToUse)
				currentInteraction := interaction
				httpmock.RegisterResponder("GET", expectedChunkURL,
					func(req *http.Request) (*http.Response, error) {
						if req.Header.Get("Range") != currentInteraction.expectedRangeHeader {
							return httpmock.NewStringResponse(http.StatusInternalServerError,
								fmt.Sprintf("Test mock: Unexpected Range header for URL %s. Got: '%s', Expected: '%s'",
									expectedChunkURL, req.Header.Get("Range"), currentInteraction.expectedRangeHeader)), nil
						}
						resp := httpmock.NewBytesResponse(currentInteraction.responseStatus, currentInteraction.mockResponseData)
						if currentInteraction.responseContentRangeHeader != "" {
							resp.Header.Set("Content-Range", currentInteraction.responseContentRangeHeader)
						}
						return resp, nil
					},
				)
			}
			n, err := s.ReadFile(node, dest, tc.readFileOffset)

			if tc.expectError {
				if err == nil {
					t.Errorf("Expected an error, but got nil")
				} else if tc.expectedErrorMsgSubstring != "" && !strings.Contains(err.Error(), tc.expectedErrorMsgSubstring) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tc.expectedErrorMsgSubstring, err.Error())
				}
			} else {
				if err != nil {
					mockInfo := httpmock.GetCallCountInfo()
					var calledUrls []string
					for u := range mockInfo {
						if mockInfo[u] > 0 {
							calledUrls = append(calledUrls, u)
						}
					}
					t.Fatalf("ReadFile failed unexpectedly: %v. Called URLs: %v", err, calledUrls)
				}
			}

			if !tc.expectError {
				if n != tc.expectedBytesRead {
					t.Errorf("Expected to read %d bytes, got %d", tc.expectedBytesRead, n)
				}

				// Prepare expected destination buffer content for comparison
				// Only compare the part of dest that should have been written to.
				var relevantDestSlice []byte
				if tc.destBufSize > 0 && tc.expectedBytesRead > 0 && tc.expectedBytesRead <= tc.destBufSize {
					relevantDestSlice = dest[:tc.expectedBytesRead]
				} else if tc.expectedBytesRead == 0 {
					relevantDestSlice = make([]byte, 0)
				} else {
					relevantDestSlice = dest
				}

				if !reflect.DeepEqual(relevantDestSlice, tc.expectedDestContent) {
					t.Errorf("Destination buffer content mismatch (comparing up to expectedBytesRead).\nGot: %v\nExp: %v\nFull Dest: %v", relevantDestSlice, tc.expectedDestContent, dest)
				}
			}

			// Verify HTTP call counts
			callInfo := httpmock.GetCallCountInfo()
			if len(tc.expectedHTTPInteractions) > 0 {
				for _, interaction := range tc.expectedHTTPInteractions {
					expectedChunkURL := fmt.Sprintf("%s/%s/chunks/%s", cdnBaseURL, tc.imageIDToUse, interaction.chunkNameToUse)
					getCallKey := fmt.Sprintf("GET %s", expectedChunkURL)
					if callInfo[getCallKey] != 1 {
						t.Errorf("Expected 1 call to chunk URL '%s', got %d. All calls: %v", expectedChunkURL, callInfo[getCallKey], callInfo)
					}
				}
				// Check if total calls match expected interactions, helps catch unexpected extra calls
				if httpmock.GetTotalCallCount() != len(tc.expectedHTTPInteractions) {
					t.Errorf("Expected total of %d HTTP calls, but got %d. All calls: %v", len(tc.expectedHTTPInteractions), httpmock.GetTotalCallCount(), callInfo)
				}
			}
		})
	}
}
