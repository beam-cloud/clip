package archive

var magic []byte = []byte{0x89, 0x43, 0x4C, 0x49, 0x50, 0x0D, 0x0A, 0x1A, 0x0A}

type ClipArchiveHeader struct {
	Magic             []byte
	ClipFormatVersion uint8
	IndexSize         uint16
}

type ClipArchiveFile struct {
	Header ClipArchiveHeader
	Index  []byte
	Blocks []Block
}

type Block struct {
	filePath  string
	size      uint16
	buffer    []byte
	blockType BlockType
}

type BlockType byte

const (
	blockTypeData BlockType = iota
	blockTypeStartOfFile
	blockTypeEndOfFile
	blockTypeDirectory
	blockTypeChecksum
)
