package archive

import "os"

var magic []byte = []byte{0x89, 0x43, 0x4C, 0x49, 0x50, 0x0D, 0x0A, 0x1A, 0x0A}

type ClipFileHeader struct {
	Magic []byte
}

type blockType byte

const (
	blockTypeData blockType = iota
	blockTypeStartOfFile
	blockTypeEndOfFile
	blockTypeDirectory
	blockTypeChecksum
)

type Block struct {
	filePath  string
	numBytes  uint16
	buffer    []byte
	blockType blockType
	uid       int
	gid       int
	mode      os.FileMode
}
