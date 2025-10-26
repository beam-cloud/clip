package storage

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/metrics"
)

// OCILayoutClipStorage implements ClipStorageInterface for local OCI layout directories
type OCILayoutClipStorage struct {
	metadata    *common.ClipArchiveMetadata
	storageInfo *common.OCILayoutStorageInfo
}

// OCILayoutClipStorageOpts contains options for creating OCI layout storage
type OCILayoutClipStorageOpts struct {
	LayoutPath string
	Tag        string
}

// NewOCILayoutClipStorage creates a new OCI layout storage backend
func NewOCILayoutClipStorage(metadata *common.ClipArchiveMetadata, opts OCILayoutClipStorageOpts) (*OCILayoutClipStorage, error) {
	storageInfo, ok := metadata.StorageInfo.(*common.OCILayoutStorageInfo)
	if !ok {
		return nil, fmt.Errorf("invalid storage info type for OCI layout storage")
	}

	return &OCILayoutClipStorage{
		metadata:    metadata,
		storageInfo: storageInfo,
	}, nil
}

func (olcs *OCILayoutClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	// Check if this is a remote file
	if node.Remote == nil {
		// Legacy file or directory - not supported in OCI layout mode
		return 0, fmt.Errorf("legacy file access not supported in OCI layout mode")
	}

	// Get gzip index for this layer
	gzipIndex, exists := olcs.storageInfo.GzipIdxByLayer[node.Remote.LayerDigest]
	if !exists {
		return 0, fmt.Errorf("no gzip index found for layer %s", node.Remote.LayerDigest)
	}

	// Calculate what we want to read
	wantUStart := node.Remote.UOffset + offset
	wantUEnd := node.Remote.UOffset + node.Remote.ULength
	readLen := int64(len(dest))
	
	if wantUStart+readLen > wantUEnd {
		readLen = wantUEnd - wantUStart
	}
	
	if readLen <= 0 {
		return 0, nil
	}

	// Find nearest checkpoint
	cStart, cU := nearestLayoutCheckpoint(gzipIndex.Checkpoints, wantUStart)

	// Open layer blob file
	blobPath := filepath.Join(olcs.storageInfo.LayoutPath, "blobs", strings.ReplaceAll(node.Remote.LayerDigest, ":", "/"))
	file, err := os.Open(blobPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open layer blob: %w", err)
	}
	defer file.Close()

	// Seek to compressed offset
	if _, err := file.Seek(cStart, io.SeekStart); err != nil {
		return 0, fmt.Errorf("failed to seek to compressed offset: %w", err)
	}

	// Decompress and seek to the desired position
	startTime := time.Now()
	inflateStart := time.Now()
	n, err := olcs.decompressAndRead(file, cU, wantUStart, dest[:readLen])
	
	// Record metrics
	metrics.RecordRangeGet(node.Remote.LayerDigest, readLen, time.Since(startTime))
	metrics.RecordInflation(time.Since(inflateStart))
	metrics.RecordRead(int64(n), false) // Always a miss for local storage
	
	return n, err
}

func (olcs *OCILayoutClipStorage) decompressAndRead(file *os.File, cU, wantUStart int64, dest []byte) (int, error) {
	// Create gzip reader
	gzr, err := gzip.NewReader(file)
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()
	
	// Skip bytes from checkpoint to desired position
	skipBytes := wantUStart - cU
	if skipBytes > 0 {
		_, err := io.CopyN(io.Discard, gzr, skipBytes)
		if err != nil {
			return 0, fmt.Errorf("failed to skip to desired position: %w", err)
		}
	}
	
	// Read the requested data
	n, err := io.ReadFull(gzr, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("failed to read data: %w", err)
	}
	
	return n, nil
}

func (olcs *OCILayoutClipStorage) Metadata() *common.ClipArchiveMetadata {
	return olcs.metadata
}

func (olcs *OCILayoutClipStorage) CachedLocally() bool {
	return true // OCI layout storage is local
}

func (olcs *OCILayoutClipStorage) Cleanup() error {
	// No cleanup needed for OCI layout storage
	return nil
}

// nearestLayoutCheckpoint finds the largest checkpoint UOff <= wantU
func nearestLayoutCheckpoint(checkpoints []common.GzipCheckpoint, wantU int64) (cOff, uOff int64) {
	if len(checkpoints) == 0 {
		return 0, 0
	}
	
	// Binary search for largest UOff <= wantU
	i := 0
	for j := len(checkpoints); i < j; {
		h := i + (j-i)/2
		if checkpoints[h].UOff <= wantU {
			i = h + 1
		} else {
			j = h
		}
	}
	
	if i > 0 {
		i--
	}
	
	return checkpoints[i].COff, checkpoints[i].UOff
}