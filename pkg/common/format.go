package common

import (
	"bytes"
	"encoding/gob"
	"time"
)

var ClipFileStartBytes []byte = []byte{0x89, 0x43, 0x4C, 0x49, 0x50, 0x0D, 0x0A, 0x1A, 0x0A}

const (
	ClipHeaderLength            = 54
	ClipFileFormatVersion uint8 = 0x01
	ClipChecksumLength    int64 = 8
)

type ClipArchiveHeader struct {
	StartBytes            [9]byte
	ClipFileFormatVersion uint8
	IndexLength           int64
	IndexPos              int64
	StorageInfoLength     int64
	StorageInfoPos        int64
	StorageInfoType       [12]byte
}

/*

Data files are stored inside a clip in this format:

	BlockType BlockType
	Data      []byte
	Checksum  []byte

*/

type BlockType byte

const (
	BlockTypeFile BlockType = iota
)

// Storage info is a structure containing data describing remote storage config
type StorageInfoWrapper struct {
	Type string
	Data []byte
}

type ClipStorageInfo interface {
	Type() string
	Encode() ([]byte, error)
}

// Storage Info Implementations
type S3StorageInfo struct {
	Bucket         string
	Region         string
	Key            string
	Endpoint       string
	ForcePathStyle bool
}

func (ssi S3StorageInfo) Type() string {
	return string(StorageModeS3)
}

func (ssi S3StorageInfo) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(ssi); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// LayerMetadata contains information about an individual OCI layer
type LayerMetadata struct {
	MIMEType    string            `json:"MIMEType"`
	Digest      string            `json:"Digest"`
	Size        int64             `json:"Size"`
	Annotations map[string]string `json:"Annotations,omitempty"`
}

// ImageMetadata contains comprehensive metadata about the OCI image
// This is embedded in the index to avoid runtime lookups
type ImageMetadata struct {
	// Image identification
	Name   string `json:"Name"`   // Full image reference (e.g., docker.io/library/alpine:3.18)
	Digest string `json:"Digest"` // Image manifest digest

	// Image configuration
	RepoTags      []string          `json:"RepoTags,omitempty"`
	Created       time.Time         `json:"Created"`
	DockerVersion string            `json:"DockerVersion,omitempty"`
	Labels        map[string]string `json:"Labels,omitempty"`
	Architecture  string            `json:"Architecture"`
	Os            string            `json:"Os"`
	Variant       string            `json:"Variant,omitempty"`
	Author        string            `json:"Author,omitempty"`

	// Runtime configuration
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	User         string              `json:"User,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`

	// Layer information
	Layers     []string        `json:"Layers"`     // Layer digests
	LayersData []LayerMetadata `json:"LayersData"` // Detailed layer information
}

// OCIStorageInfo stores metadata for OCI images with decompression indexes
type OCIStorageInfo struct {
	RegistryURL             string
	Repository              string
	Reference               string // tag or digest
	Layers                  []string
	GzipIdxByLayer          map[string]*GzipIndex // per-layer gzip decompression index
	ZstdIdxByLayer          map[string]*ZstdIndex // per-layer zstd index (P1)
	DecompressedHashByLayer map[string]string     // maps layer digest -> SHA256 hash of decompressed data
	ImageMetadata           *ImageMetadata        `json:"ImageMetadata,omitempty"` // Image metadata - embedded to avoid runtime lookups
}

func (osi OCIStorageInfo) Type() string {
	return "oci"
}

func (osi OCIStorageInfo) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(osi); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
