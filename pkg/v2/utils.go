package clipv2

import (
	"fmt"

	common "github.com/beam-cloud/clip/pkg/common"
)

func validateReadFileInput(node *common.ClipNode, off int64, dest []byte) error {
	if node.NodeType != common.FileNode {
		return fmt.Errorf("cannot ReadFile on non-file node type: %s", node.NodeType)
	}

	if off < 0 {
		return fmt.Errorf("negative offset %d is invalid", off)
	}

	if len(dest) > int(node.DataLen) {
		return fmt.Errorf("destination buffer size %d is larger than node data length %d", len(dest), node.DataLen)
	}
	return nil
}

func getChunkIndices(startOffset int64, chunkSize int64, endOffset int64, chunks []string) (int64, int64, error) {
	startChunk := startOffset / chunkSize
	endChunk := (endOffset - 1) / chunkSize
	if endChunk+1 > int64(len(chunks)) || startChunk < 0 || startChunk > endChunk {
		return 0, 0, fmt.Errorf("invalid chunk indices for %d chunks: startChunk %d, endChunk %d", len(chunks), startChunk, endChunk+1)
	}
	return startChunk, endChunk, nil
}
