package storage

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/metrics"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/rs/zerolog/log"
)

// BlobFetcher interface for fetching blob data from registry
type BlobFetcher interface {
	RangeGet(layerDigest string, cStart int64) (io.ReadCloser, error)
}

// OCIClipStorage implements ClipStorageInterface for OCI images
type OCIClipStorage struct {
	metadata    *common.ClipArchiveMetadata
	storageInfo *common.OCIStorageInfo
	fetcher     BlobFetcher
}

// OCIClipStorageOpts contains options for creating OCI storage
type OCIClipStorageOpts struct {
	RegistryURL    string
	Repository     string
	AuthConfigPath string
}

// NewOCIClipStorage creates a new OCI storage backend
func NewOCIClipStorage(metadata *common.ClipArchiveMetadata, opts OCIClipStorageOpts) (*OCIClipStorage, error) {
	storageInfo, ok := metadata.StorageInfo.(*common.OCIStorageInfo)
	if !ok {
		return nil, fmt.Errorf("invalid storage info type for OCI storage")
	}

	// Create blob fetcher
	fetcher, err := NewRegistryBlobFetcher(storageInfo.RegistryURL, storageInfo.Repository, storageInfo.AuthConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob fetcher: %w", err)
	}

	return &OCIClipStorage{
		metadata:    metadata,
		storageInfo: storageInfo,
		fetcher:     fetcher,
	}, nil
}

func (ocs *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
	// Check if this is a remote file
	if node.Remote == nil {
		// Legacy file or directory - not supported in OCI mode
		return 0, fmt.Errorf("legacy file access not supported in OCI mode")
	}

	// Get gzip index for this layer
	gzipIndex, exists := ocs.storageInfo.GzipIdxByLayer[node.Remote.LayerDigest]
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
	cStart, cU := nearestCheckpoint(gzipIndex.Checkpoints, wantUStart)

	// Get compressed stream starting from checkpoint
	startTime := time.Now()
	rc, err := ocs.fetcher.RangeGet(node.Remote.LayerDigest, cStart)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch blob range: %w", err)
	}
	defer rc.Close()

	// Decompress and seek to the desired position
	inflateStart := time.Now()
	n, err := ocs.decompressAndRead(rc, cU, wantUStart, dest[:readLen])
	
	// Record metrics
	metrics.RecordRangeGet(node.Remote.LayerDigest, readLen, time.Since(startTime))
	metrics.RecordInflation(time.Since(inflateStart))
	metrics.RecordRead(int64(n), false) // Always a miss for OCI storage
	
	return n, err
}

func (ocs *OCIClipStorage) decompressAndRead(rc io.ReadCloser, cU, wantUStart int64, dest []byte) (int, error) {
	// Create gzip reader
	gzr, err := gzip.NewReader(rc)
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

func (ocs *OCIClipStorage) Metadata() *common.ClipArchiveMetadata {
	return ocs.metadata
}

func (ocs *OCIClipStorage) CachedLocally() bool {
	return false // OCI storage is always remote
}

func (ocs *OCIClipStorage) Cleanup() error {
	// No cleanup needed for OCI storage
	return nil
}

// nearestCheckpoint finds the largest checkpoint UOff <= wantU (copied from archive.go)
func nearestCheckpoint(checkpoints []common.GzipCheckpoint, wantU int64) (cOff, uOff int64) {
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

// RegistryBlobFetcher implements BlobFetcher for container registries
type RegistryBlobFetcher struct {
	registryURL    string
	repository     string
	authConfigPath string
	client         *http.Client
}

// NewRegistryBlobFetcher creates a new registry blob fetcher
func NewRegistryBlobFetcher(registryURL, repository, authConfigPath string) (*RegistryBlobFetcher, error) {
	return &RegistryBlobFetcher{
		registryURL:    registryURL,
		repository:     repository,
		authConfigPath: authConfigPath,
		client:         &http.Client{},
	}, nil
}

func (rbf *RegistryBlobFetcher) RangeGet(layerDigest string, cStart int64) (io.ReadCloser, error) {
	// Construct blob URL
	blobURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", rbf.registryURL, rbf.repository, layerDigest)
	
	// Create request with Range header
	req, err := http.NewRequest("GET", blobURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	// Add Range header for partial content
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", cStart))
	
	// Add authentication if available
	if err := rbf.addAuth(req); err != nil {
		log.Warn().Err(err).Msg("failed to add authentication, proceeding without auth")
	}
	
	// Make request
	resp, err := rbf.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	
	// Check response status
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	
	return resp.Body, nil
}

func (rbf *RegistryBlobFetcher) addAuth(req *http.Request) error {
	// Try to load authentication from Docker config
	if rbf.authConfigPath == "" {
		// Try default locations
		homeDir, err := os.UserHomeDir()
		if err == nil {
			rbf.authConfigPath = filepath.Join(homeDir, ".docker", "config.json")
		}
	}
	
	if rbf.authConfigPath == "" {
		return fmt.Errorf("no auth config path specified")
	}
	
	// Check if config file exists
	if _, err := os.Stat(rbf.authConfigPath); os.IsNotExist(err) {
		return fmt.Errorf("auth config file not found: %s", rbf.authConfigPath)
	}
	
	// Parse registry name for authentication
	registry := rbf.registryURL
	if registry == "docker.io" || registry == "index.docker.io" {
		registry = "https://index.docker.io/v1/"
	}
	
	// Create registry reference for authentication
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:latest", rbf.registryURL, rbf.repository))
	if err != nil {
		return fmt.Errorf("failed to parse registry reference: %w", err)
	}
	
	// Get authenticator
	auth, err := authn.DefaultKeychain.Resolve(ref.Context().Registry)
	if err != nil {
		return fmt.Errorf("failed to resolve authentication: %w", err)
	}
	
	// Get authorization header
	authConfig, err := auth.Authorization()
	if err != nil {
		return fmt.Errorf("failed to get authorization: %w", err)
	}
	
	// Add authorization header
	if authConfig.Username != "" && authConfig.Password != "" {
		req.SetBasicAuth(authConfig.Username, authConfig.Password)
	} else if authConfig.Auth != "" {
		req.Header.Set("Authorization", "Basic "+authConfig.Auth)
	} else if authConfig.RegistryToken != "" {
		req.Header.Set("Authorization", "Bearer "+authConfig.RegistryToken)
	}
	
	return nil
}