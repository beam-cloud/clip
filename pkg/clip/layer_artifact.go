package clip

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"strings"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/tidwall/btree"
)

// LayerArtifactVersion is baked into layer-index cache keys so that any change
// to the artifact format or indexing semantics invalidates previously cached
// artifacts.
const LayerArtifactVersion = 1

// LayerEntryKind describes the type of operation a layer entry applies to the
// merged image index.
type LayerEntryKind uint8

const (
	// LayerEntryNode adds (or replaces) a file/dir/symlink node.
	LayerEntryNode LayerEntryKind = iota
	// LayerEntryWhiteout removes a path (and any children) added by lower layers.
	LayerEntryWhiteout
	// LayerEntryOpaqueWhiteout removes all children of a directory added by lower layers.
	LayerEntryOpaqueWhiteout
	// LayerEntryHardLink adds a node whose attributes are copied from an
	// existing path in the index at merge time.
	LayerEntryHardLink
)

// LayerEntry is a single ordered operation recorded while walking a layer tar
// stream. Replaying entries in order against an index reproduces exactly the
// same result as indexing the layer directly.
type LayerEntry struct {
	Kind   LayerEntryKind
	Node   *common.ClipNode // set for LayerEntryNode
	Path   string           // whiteout victim, opaque dir, or hardlink path
	Target string           // hardlink target path
}

// LayerArtifact is a self-contained, serializable result of indexing a single
// OCI layer. It is fully deterministic given the compressed layer blob and the
// checkpoint interval, which makes it safe to cache keyed by layer digest.
type LayerArtifact struct {
	Version          int
	LayerDigest      string
	CheckpointMiB    int64
	Entries          []LayerEntry
	Checkpoints      []common.GzipCheckpoint
	DecompressedHash string
	UncompressedSize int64
}

// LayerArtifactCacheKey returns the deterministic cache key for a layer's
// index artifact. The key incorporates the artifact format version and the
// checkpoint interval, since both affect artifact contents.
func LayerArtifactCacheKey(layerDigest string, checkpointMiB int64) string {
	digest := strings.ReplaceAll(layerDigest, ":", "_")
	return fmt.Sprintf("clip-layer-index/v%d/cp%d/%s", LayerArtifactVersion, checkpointMiB, digest)
}

// EncodeLayerArtifact serializes an artifact with gob. The artifact contains
// only slices and fixed-size structs (no maps), so encoding is deterministic.
func EncodeLayerArtifact(artifact *LayerArtifact) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(artifact); err != nil {
		return nil, fmt.Errorf("failed to encode layer artifact: %w", err)
	}
	return buf.Bytes(), nil
}

// DecodeLayerArtifact deserializes an artifact and validates it against the
// expected layer digest and checkpoint interval. Returns an error for any
// mismatch so callers can treat corrupt/stale cache entries as misses.
func DecodeLayerArtifact(data []byte, expectedDigest string, expectedCheckpointMiB int64) (*LayerArtifact, error) {
	var artifact LayerArtifact
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&artifact); err != nil {
		return nil, fmt.Errorf("failed to decode layer artifact: %w", err)
	}
	if artifact.Version != LayerArtifactVersion {
		return nil, fmt.Errorf("layer artifact version mismatch: got %d, want %d", artifact.Version, LayerArtifactVersion)
	}
	if artifact.LayerDigest != expectedDigest {
		return nil, fmt.Errorf("layer artifact digest mismatch: got %s, want %s", artifact.LayerDigest, expectedDigest)
	}
	if artifact.CheckpointMiB != expectedCheckpointMiB {
		return nil, fmt.Errorf("layer artifact checkpoint interval mismatch: got %d, want %d", artifact.CheckpointMiB, expectedCheckpointMiB)
	}
	if artifact.DecompressedHash == "" {
		return nil, fmt.Errorf("layer artifact missing decompressed hash")
	}
	return &artifact, nil
}

// applyLayerArtifact replays a layer's entries, in order, against the shared
// image index. This reproduces the exact semantics of indexing the layer
// directly: whiteouts delete lower-layer paths, later entries replace earlier
// ones, and hard links copy attributes from the current index state.
func (ca *ClipArchiver) applyLayerArtifact(index *btree.BTree, artifact *LayerArtifact) {
	for i := range artifact.Entries {
		entry := &artifact.Entries[i]
		switch entry.Kind {
		case LayerEntryNode:
			if entry.Node != nil {
				index.Set(entry.Node)
			}
		case LayerEntryWhiteout:
			ca.deleteNode(index, entry.Path)
		case LayerEntryOpaqueWhiteout:
			ca.deleteRange(index, entry.Path+"/")
		case LayerEntryHardLink:
			targetNode := index.Get(&common.ClipNode{Path: entry.Target})
			if targetNode != nil {
				tn := targetNode.(*common.ClipNode)
				index.Set(&common.ClipNode{
					Path:     entry.Path,
					NodeType: common.FileNode,
					Attr:     tn.Attr,
					Remote:   tn.Remote,
				})
			}
		}
	}
}
