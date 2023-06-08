package archive

var ClipFileStartBytes []byte = []byte{0x89, 0x43, 0x4C, 0x49, 0x50, 0x0D, 0x0A, 0x1A, 0x0A}

const (
	ClipHeaderLength            = 109
	ClipFileFormatVersion uint8 = 0x01
)

type ClipArchiveHeader struct {
	StartBytes            []byte
	ClipFileFormatVersion uint8
	IndexSize             int
}

/*

Data files are stored inside a clip in this format:

	BlockType BlockType
	Data      []byte
	Checksum  []byte

*/

type BlockType byte

const (
	blockTypeFile BlockType = iota
)
