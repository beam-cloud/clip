package archive

import (
	"encoding/binary"
	"hash/crc64"
)

func computeChecksum(data []byte) []byte {
	table := crc64.MakeTable(crc64.ISO)
	checksum := crc64.Checksum(data, table)

	// Convert uint64 to byte slice
	checksumBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(checksumBytes, checksum)

	return checksumBytes
}
