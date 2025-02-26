package common

import (
	"bytes"
	"encoding/gob"
)

var ClipFileStartBytes []byte = []byte{0x89, 0x43, 0x4C, 0x49, 0x50, 0x0D, 0x0A, 0x1A, 0x0A}

const (
	ClipHeaderLength            = 54
	ClipFileFormatVersion uint8 = 0x01
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
	return "s3"
}

func (ssi S3StorageInfo) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(ssi); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
