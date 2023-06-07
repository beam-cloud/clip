package archive

import "os"

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

// CLIP file header
//
//	0x43, 0x4C, 0x49, 0x50 => CLIP
var ClipFileHeader = []byte{0x89, 0x43, 0x4C, 0x49, 0x50, 0x0D, 0x0A, 0x1A, 0x0A}
