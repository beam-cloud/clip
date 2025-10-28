package clip

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
	"github.com/tidwall/btree"
)

// IndexOCIImageOptions configures the OCI indexer
type IndexOCIImageOptions struct {
	ImageRef       string
	CheckpointMiB  int64  // Checkpoint every N MiB (default 2)
	Verbose        bool
	AuthConfig     string // optional base64-encoded auth config
}

// countingReader tracks bytes read from an io.Reader
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	k, err := cr.r.Read(p)
	cr.n += int64(k)
	return k, err
}

// gzipIndexBuilder builds a decompression index while inflating gzip
type gzipIndexBuilder struct {
	compressedReader   *countingReader // tracks compressed offset
	uncompressedOffset int64           // tracks uncompressed offset
	checkpoints        []common.GzipCheckpoint
	checkpointInterval int64 // in bytes
	lastCheckpoint     int64 // uncompressed offset of last checkpoint
}

func newGzipIndexBuilder(cr *countingReader, checkpointMiB int64) *gzipIndexBuilder {
	return &gzipIndexBuilder{
		compressedReader:   cr,
		uncompressedOffset: 0,
		checkpoints:        make([]common.GzipCheckpoint, 0),
		checkpointInterval: checkpointMiB * 1024 * 1024,
		lastCheckpoint:     0,
	}
}

func (gib *gzipIndexBuilder) Read(p []byte) (int, error) {
	// Read from the wrapped reader (this will be the decompressed stream)
	// Note: This is called by tar.Reader, so it's already decompressed
	n := len(p)
	
	// Check if we should add a checkpoint
	if gib.uncompressedOffset-gib.lastCheckpoint >= gib.checkpointInterval {
		gib.addCheckpoint()
	}
	
	gib.uncompressedOffset += int64(n)
	return n, nil
}

func (gib *gzipIndexBuilder) addCheckpoint() {
	cp := common.GzipCheckpoint{
		COff: gib.compressedReader.n,
		UOff: gib.uncompressedOffset,
	}
	gib.checkpoints = append(gib.checkpoints, cp)
	gib.lastCheckpoint = gib.uncompressedOffset
	log.Debug().Msgf("Added checkpoint: COff=%d, UOff=%d", cp.COff, cp.UOff)
}

func (gib *gzipIndexBuilder) finalizeIndex(layerDigest string) *common.GzipIndex {
	// Add final checkpoint if needed
	if gib.uncompressedOffset > gib.lastCheckpoint {
		gib.addCheckpoint()
	}
	
	return &common.GzipIndex{
		LayerDigest: layerDigest,
		Checkpoints: gib.checkpoints,
	}
}

// IndexOCIImage creates a metadata-only index from an OCI image
func (ca *ClipArchiver) IndexOCIImage(ctx context.Context, opts IndexOCIImageOptions) (
	index *btree.BTree,
	layerDigests []string,
	gzipIdx map[string]*common.GzipIndex,
	registryURL string,
	repository string,
	reference string,
	err error,
) {
	if opts.CheckpointMiB == 0 {
		opts.CheckpointMiB = 2 // default
	}

	// Parse image reference
	ref, err := name.ParseReference(opts.ImageRef)
	if err != nil {
		return nil, nil, nil, "", "", "", fmt.Errorf("failed to parse image reference: %w", err)
	}

	// Extract registry and repository info
	registryURL = ref.Context().RegistryStr()
	repository = ref.Context().RepositoryStr()
	reference = ref.Identifier()

	// Fetch image
	img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx))
	if err != nil {
		return nil, nil, nil, "", "", "", fmt.Errorf("failed to fetch image: %w", err)
	}

	// Get image layers
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, nil, "", "", "", fmt.Errorf("failed to get layers: %w", err)
	}

	// Initialize index and maps
	index = ca.newIndex()
	layerDigests = make([]string, 0, len(layers))
	gzipIdx = make(map[string]*common.GzipIndex)

	// Create root node
	root := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Ino:  1,
			Mode: uint32(syscall.S_IFDIR | 0755),
		},
	}
	index.Set(root)

	log.Info().Msgf("Indexing %d layers from %s", len(layers), opts.ImageRef)

	// Process each layer in order (bottom to top)
	for i, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return nil, nil, nil, "", "", "", fmt.Errorf("failed to get layer digest: %w", err)
		}

		layerDigestStr := digest.String()
		layerDigests = append(layerDigests, layerDigestStr)

		log.Info().Msgf("Processing layer %d/%d: %s", i+1, len(layers), layerDigestStr)

		// Get compressed layer stream
		compressedRC, err := layer.Compressed()
		if err != nil {
			return nil, nil, nil, "", "", "", fmt.Errorf("failed to get compressed layer: %w", err)
		}

		// Index this layer with optimizations
		gzipIndex, err := ca.indexLayerOptimized(ctx, compressedRC, layerDigestStr, index, opts)
		compressedRC.Close()
		if err != nil {
			return nil, nil, nil, "", "", "", fmt.Errorf("failed to index layer %s: %w", layerDigestStr, err)
		}

		gzipIdx[layerDigestStr] = gzipIndex
	}

	log.Info().Msgf("Successfully indexed image with %d files", index.Len())

	return index, layerDigests, gzipIdx, registryURL, repository, reference, nil
}

// indexLayerOptimized processes a single layer with optimizations
func (ca *ClipArchiver) indexLayerOptimized(
	ctx context.Context,
	compressedRC io.ReadCloser,
	layerDigest string,
	index *btree.BTree,
	opts IndexOCIImageOptions,
) (*common.GzipIndex, error) {
	// Wrap compressed stream with counting reader
	compressedCounter := &countingReader{r: compressedRC}

	// Create gzip reader
	gzr, err := gzip.NewReader(compressedCounter)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Wrap uncompressed stream with counting reader
	uncompressedCounter := &countingReader{r: gzr}

	// Create tar reader
	tr := tar.NewReader(uncompressedCounter)

	// Track checkpoints
	checkpoints := make([]common.GzipCheckpoint, 0)
	checkpointInterval := opts.CheckpointMiB * 1024 * 1024
	lastCheckpoint := int64(0)

	// Process tar entries
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar header: %w", err)
		}

		// Record checkpoint periodically (before processing file data)
		if uncompressedCounter.n-lastCheckpoint >= checkpointInterval {
			cp := common.GzipCheckpoint{
				COff: compressedCounter.n,
				UOff: uncompressedCounter.n,
			}
			checkpoints = append(checkpoints, cp)
			lastCheckpoint = uncompressedCounter.n
			log.Debug().Msgf("Added checkpoint: COff=%d, UOff=%d", cp.COff, cp.UOff)
		}

		// Clean path
		cleanPath := path.Clean("/" + strings.TrimPrefix(hdr.Name, "./"))

		// Handle whiteouts
		if ca.handleWhiteout(index, cleanPath) {
			continue
		}

		// Process based on type
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			dataStart := uncompressedCounter.n
			
			// OPTIMIZATION: Skip file content efficiently
			// Use CopyN with exact size instead of Copy which reads until EOF
			if hdr.Size > 0 {
				n, err := io.CopyN(io.Discard, tr, hdr.Size)
				if err != nil && err != io.EOF {
					return nil, fmt.Errorf("failed to skip file content: %w", err)
				}
				if n != hdr.Size {
					return nil, fmt.Errorf("failed to skip complete file (wanted %d, got %d)", hdr.Size, n)
				}
			}

			// CRITICAL: Ensure parent directories exist BEFORE creating file
			ca.ensureParentDirs(index, cleanPath, layerDigest, hdr)

			// Compute content hash from the layer digest and file path
			// This provides a stable identifier for the file
			hash := sha256.New()
			hash.Write([]byte(layerDigest))
			hash.Write([]byte(cleanPath))
			contentHash := hex.EncodeToString(hash.Sum(nil))

			node := &common.ClipNode{
				Path:        cleanPath,
				NodeType:    common.FileNode,
				ContentHash: contentHash,
				Attr: fuse.Attr{
					Ino:   ca.generateInode(layerDigest, cleanPath),
					Size:  uint64(hdr.Size),
					Mode:  ca.tarModeToFuse(hdr.Mode, tar.TypeReg),
					Atime: uint64(hdr.AccessTime.Unix()),
					Mtime: uint64(hdr.ModTime.Unix()),
					Ctime: uint64(hdr.ChangeTime.Unix()),
					Owner: fuse.Owner{
						Uid: uint32(hdr.Uid),
						Gid: uint32(hdr.Gid),
					},
				},
				Remote: &common.RemoteRef{
					LayerDigest: layerDigest,
					UOffset:     dataStart,
					ULength:     hdr.Size,
				},
			}

			index.Set(node)

			if opts.Verbose {
				log.Debug().Msgf("  File: %s (size=%d, uoff=%d)", cleanPath, hdr.Size, dataStart)
			}

		case tar.TypeSymlink:
			// Get the symlink target
			target := hdr.Linkname
			if target == "" {
				log.Warn().Msgf("Empty symlink target for %s", cleanPath)
			}
			
			// CRITICAL: Ensure parent directories exist BEFORE creating symlink
			ca.ensureParentDirs(index, cleanPath, layerDigest, hdr)
			
			node := &common.ClipNode{
				Path:     cleanPath,
				NodeType: common.SymLinkNode,
				Target:   target,
				Attr: fuse.Attr{
					Ino:   ca.generateInode(layerDigest, cleanPath),
					Size:  uint64(len(target)), // CRITICAL: Symlink size must be length of target
					Mode:  ca.tarModeToFuse(hdr.Mode, tar.TypeSymlink),
					Atime: uint64(hdr.AccessTime.Unix()),
					Mtime: uint64(hdr.ModTime.Unix()),
					Ctime: uint64(hdr.ChangeTime.Unix()),
					Owner: fuse.Owner{
						Uid: uint32(hdr.Uid),
						Gid: uint32(hdr.Gid),
					},
				},
			}

			index.Set(node)

			if opts.Verbose {
				log.Debug().Msgf("  Symlink: %s -> %s", cleanPath, target)
			}

		case tar.TypeDir:
			// Skip special runtime directories that should be mounted by the container runtime
			// These directories (/proc, /sys, /dev) cause conflicts when runc tries to mount them
			if ca.isRuntimeDirectory(cleanPath) {
				if opts.Verbose {
					log.Debug().Msgf("  Skipping runtime dir: %s", cleanPath)
				}
				continue
			}

			// Ensure parent directories exist
			ca.ensureParentDirs(index, cleanPath, layerDigest, hdr)
			
			node := &common.ClipNode{
				Path:     cleanPath,
				NodeType: common.DirNode,
				Attr: fuse.Attr{
					Ino:   ca.generateInode(layerDigest, cleanPath),
					Mode:  ca.tarModeToFuse(hdr.Mode, tar.TypeDir),
					Atime: uint64(hdr.AccessTime.Unix()),
					Mtime: uint64(hdr.ModTime.Unix()),
					Ctime: uint64(hdr.ChangeTime.Unix()),
					Owner: fuse.Owner{
						Uid: uint32(hdr.Uid),
						Gid: uint32(hdr.Gid),
					},
				},
			}

			index.Set(node)

			if opts.Verbose {
				log.Debug().Msgf("  Dir: %s", cleanPath)
			}

		case tar.TypeLink:
			// Hard links: point to the same inode as the target
			targetPath := path.Clean("/" + strings.TrimPrefix(hdr.Linkname, "./"))
			targetNode := index.Get(&common.ClipNode{Path: targetPath})
			if targetNode != nil {
				// Ensure parent directories exist BEFORE creating hard link
				ca.ensureParentDirs(index, cleanPath, layerDigest, hdr)
				
				tn := targetNode.(*common.ClipNode)
				node := &common.ClipNode{
					Path:        cleanPath,
					NodeType:    common.FileNode,
					ContentHash: tn.ContentHash,
					Attr:        tn.Attr, // Share inode with target
					Remote:      tn.Remote,
				}
				index.Set(node)
			}
		}
	}

	// Add final checkpoint if needed
	if uncompressedCounter.n > lastCheckpoint {
		cp := common.GzipCheckpoint{
			COff: compressedCounter.n,
			UOff: uncompressedCounter.n,
		}
		checkpoints = append(checkpoints, cp)
		log.Debug().Msgf("Added final checkpoint: COff=%d, UOff=%d", cp.COff, cp.UOff)
	}

	// Return gzip index
	return &common.GzipIndex{
		LayerDigest: layerDigest,
		Checkpoints: checkpoints,
	}, nil
}

// handleWhiteout processes OCI whiteout files
func (ca *ClipArchiver) handleWhiteout(index *btree.BTree, fullPath string) bool {
	dir := path.Dir(fullPath)
	base := path.Base(fullPath)

	// Opaque whiteout: .wh..wh..opq
	if base == ".wh..wh..opq" {
		// Remove all entries under this directory from lower layers
		ca.deleteRange(index, dir+"/")
		log.Debug().Msgf("  Opaque whiteout: %s", dir)
		return true
	}

	// Regular whiteout: .wh.<name>
	if strings.HasPrefix(base, ".wh.") {
		victim := path.Join(dir, strings.TrimPrefix(base, ".wh."))
		ca.deleteNode(index, victim)
		log.Debug().Msgf("  Whiteout: %s", victim)
		return true
	}

	return false
}

// deleteNode removes a node and all its children from the index
func (ca *ClipArchiver) deleteNode(index *btree.BTree, nodePath string) {
	// Remove the node itself
	index.Delete(&common.ClipNode{Path: nodePath})
	
	// Remove all children (for directories)
	ca.deleteRange(index, nodePath+"/")
}

// deleteRange removes all nodes with paths starting with prefix
func (ca *ClipArchiver) deleteRange(index *btree.BTree, prefix string) {
	var toDelete []*common.ClipNode
	
	pivot := &common.ClipNode{Path: prefix}
	index.Ascend(pivot, func(a interface{}) bool {
		node := a.(*common.ClipNode)
		if strings.HasPrefix(node.Path, prefix) {
			toDelete = append(toDelete, node)
			return true
		}
		return false // stop iteration once we're past the prefix
	})
	
	for _, node := range toDelete {
		index.Delete(node)
	}
}

// ensureParentDirs creates parent directory nodes if they don't exist
// This is CRITICAL for FUSE filesystem integrity - every file must have valid parent dirs
func (ca *ClipArchiver) ensureParentDirs(index *btree.BTree, filePath string, layerDigest string, hdr *tar.Header) {
	if filePath == "/" {
		return
	}
	
	parts := strings.Split(strings.Trim(filePath, "/"), "/")
	
	// Create all parent directories with proper metadata
	for i := 1; i < len(parts); i++ {
		dirPath := "/" + strings.Join(parts[:i], "/")
		
		// Check if directory already exists
		if index.Get(&common.ClipNode{Path: dirPath}) == nil {
			// Create directory node with proper metadata
			// Use the file's header times for consistency if available
			var atime, mtime, ctime uint64
			if hdr != nil {
				atime = uint64(hdr.AccessTime.Unix())
				mtime = uint64(hdr.ModTime.Unix())
				ctime = uint64(hdr.ChangeTime.Unix())
			}
			
			node := &common.ClipNode{
				Path:     dirPath,
				NodeType: common.DirNode,
				Attr: fuse.Attr{
					Ino:   ca.generateInode(layerDigest, dirPath),
					Mode:  uint32(syscall.S_IFDIR | 0755),
					Atime: atime,
					Mtime: mtime,
					Ctime: ctime,
					Owner: fuse.Owner{
						Uid: 0,
						Gid: 0,
					},
				},
			}
			index.Set(node)
		}
	}
}

// isRuntimeDirectory checks if a path is a special runtime directory
// that should be mounted by the container runtime, not included in the image
func (ca *ClipArchiver) isRuntimeDirectory(path string) bool {
	runtimeDirs := []string{
		"/proc",
		"/sys",
		"/dev",
	}
	
	for _, dir := range runtimeDirs {
		if path == dir {
			return true
		}
	}
	
	return false
}

// tarModeToFuse converts tar mode to FUSE mode
func (ca *ClipArchiver) tarModeToFuse(tarMode int64, typeflag byte) uint32 {
	mode := uint32(tarMode & 0777) // permission bits
	
	switch typeflag {
	case tar.TypeDir:
		mode |= syscall.S_IFDIR
	case tar.TypeSymlink:
		mode |= syscall.S_IFLNK
	case tar.TypeReg, tar.TypeRegA:
		mode |= syscall.S_IFREG
	default:
		mode |= syscall.S_IFREG
	}
	
	return mode
}

// generateInode creates a stable inode number from digest and path
func (ca *ClipArchiver) generateInode(digest string, path string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(digest))
	h.Write([]byte(path))
	inode := h.Sum64()
	
	// Ensure inode is never 0 (reserved for errors) or 1 (reserved for root)
	if inode <= 1 {
		inode = 2
	}
	
	return inode
}

// CreateFromOCI creates a metadata-only .clip file from an OCI image
func (ca *ClipArchiver) CreateFromOCI(ctx context.Context, opts IndexOCIImageOptions, clipOut string) error {
	// Index the OCI image
	index, layers, gzipIdx, registryURL, repository, reference, err := ca.IndexOCIImage(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to index OCI image: %w", err)
	}

	// Create OCIStorageInfo
	storageInfo := &common.OCIStorageInfo{
		RegistryURL:    registryURL,
		Repository:     repository,
		Reference:      reference,
		Layers:         layers,
		GzipIdxByLayer: gzipIdx,
		ZstdIdxByLayer: nil, // P1 feature
		AuthConfig:     opts.AuthConfig,
	}

	// Create metadata
	metadata := &common.ClipArchiveMetadata{
		Index:       index,
		StorageInfo: storageInfo,
	}

	// Write metadata-only clip file
	err = ca.CreateRemoteArchive(storageInfo, metadata, clipOut)
	if err != nil {
		return fmt.Errorf("failed to create remote archive: %w", err)
	}

	log.Info().Msgf("Created metadata-only clip file: %s", clipOut)
	log.Info().Msgf("  Files indexed: %d", index.Len())
	log.Info().Msgf("  Layers: %d", len(layers))
	
	// Calculate total checkpoint size
	totalCheckpoints := 0
	for _, idx := range gzipIdx {
		totalCheckpoints += len(idx.Checkpoints)
	}
	log.Info().Msgf("  Gzip checkpoints: %d", totalCheckpoints)

	return nil
}

// nearestCheckpoint finds the checkpoint with the largest UOff <= wantU
func nearestCheckpoint(checkpoints []common.GzipCheckpoint, wantU int64) (cOff, uOff int64) {
	if len(checkpoints) == 0 {
		return 0, 0
	}
	
	i := sort.Search(len(checkpoints), func(i int) bool {
		return checkpoints[i].UOff > wantU
	}) - 1
	
	if i < 0 {
		i = 0
	}
	
	return checkpoints[i].COff, checkpoints[i].UOff
}
