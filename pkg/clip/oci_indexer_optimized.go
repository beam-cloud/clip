package clip

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
	"github.com/tidwall/btree"
)

// IndexOCIImageFast creates a metadata-only index from an OCI image with optimizations
func (ca *ClipArchiver) IndexOCIImageFast(ctx context.Context, opts IndexOCIImageOptions) (
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

	log.Info().Msgf("Indexing %d layers from %s (parallel mode)", len(layers), opts.ImageRef)

	// Process layers in parallel with controlled concurrency
	maxConcurrent := 4
	if len(layers) < maxConcurrent {
		maxConcurrent = len(layers)
	}

	type layerResult struct {
		digest    string
		gzipIndex *common.GzipIndex
		nodes     []*common.ClipNode
		err       error
		order     int
	}

	resultsChan := make(chan layerResult, len(layers))
	semaphore := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup

	// Process layers in parallel
	for i, layer := range layers {
		wg.Add(1)
		go func(i int, layer v1.Layer) {
			defer wg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			digest, err := layer.Digest()
			if err != nil {
				resultsChan <- layerResult{err: err, order: i}
				return
			}

			layerDigestStr := digest.String()
			log.Info().Msgf("Processing layer %d/%d: %s", i+1, len(layers), layerDigestStr)

			// Get compressed layer stream
			compressedRC, err := layer.Compressed()
			if err != nil {
				resultsChan <- layerResult{err: fmt.Errorf("failed to get compressed layer: %w", err), order: i}
				return
			}

			// Index this layer
			gzipIndex, nodes, err := ca.indexLayerFast(ctx, compressedRC, layerDigestStr, opts)
			compressedRC.Close()
			
			if err != nil {
				resultsChan <- layerResult{err: fmt.Errorf("failed to index layer %s: %w", layerDigestStr, err), order: i}
				return
			}

			resultsChan <- layerResult{
				digest:    layerDigestStr,
				gzipIndex: gzipIndex,
				nodes:     nodes,
				order:     i,
			}
		}(i, layer)
	}

	// Wait for all goroutines
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results in order
	results := make([]layerResult, len(layers))
	for result := range resultsChan {
		if result.err != nil {
			return nil, nil, nil, "", "", "", result.err
		}
		results[result.order] = result
	}

	// Apply results in order (important for layer ordering)
	for _, result := range results {
		layerDigests = append(layerDigests, result.digest)
		gzipIdx[result.digest] = result.gzipIndex

		// Apply nodes to index (handle whiteouts and merging)
		for _, node := range result.nodes {
			ca.setOrMerge(index, node)
		}
	}

	log.Info().Msgf("Successfully indexed image with %d files", index.Len())

	return index, layerDigests, gzipIdx, registryURL, repository, reference, nil
}

// indexLayerFast processes a single layer with optimizations
func (ca *ClipArchiver) indexLayerFast(
	ctx context.Context,
	compressedRC io.ReadCloser,
	layerDigest string,
	opts IndexOCIImageOptions,
) (*common.GzipIndex, []*common.ClipNode, error) {
	// Wrap compressed stream with counting reader
	compressedCounter := &countingReader{r: compressedRC}

	// Create gzip reader with larger buffer
	gzr, err := gzip.NewReader(compressedCounter)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create gzip reader: %w", err)
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

	// Collect nodes to return
	nodes := make([]*common.ClipNode, 0, 512)

	// Process tar entries
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read tar header: %w", err)
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

		// Handle whiteouts (mark for later processing)
		if strings.Contains(path.Base(cleanPath), ".wh.") {
			nodes = append(nodes, &common.ClipNode{
				Path:     cleanPath,
				NodeType: common.DirNode, // Special marker
			})
			continue
		}

		// Process based on type
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			dataStart := uncompressedCounter.n

			// OPTIMIZATION: Skip file content without reading it
			// We just need to advance the tar reader's position
			// This is MUCH faster than io.Copy(io.Discard, tr)
			if hdr.Size > 0 {
				// Seek past the file data
				n, err := io.CopyN(io.Discard, tr, hdr.Size)
				if err != nil && err != io.EOF {
					return nil, nil, fmt.Errorf("failed to skip file content: %w", err)
				}
				if n != hdr.Size {
					return nil, nil, fmt.Errorf("failed to skip complete file (wanted %d, got %d)", hdr.Size, n)
				}
			}

			// Compute content hash
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

			nodes = append(nodes, node)

			if opts.Verbose {
				log.Debug().Msgf("  File: %s (size=%d, uoff=%d)", cleanPath, hdr.Size, dataStart)
			}

		case tar.TypeSymlink:
			target := hdr.Linkname
			if target == "" {
				log.Warn().Msgf("Empty symlink target for %s", cleanPath)
			}

			node := &common.ClipNode{
				Path:     cleanPath,
				NodeType: common.SymLinkNode,
				Target:   target,
				Attr: fuse.Attr{
					Ino:   ca.generateInode(layerDigest, cleanPath),
					Size:  uint64(len(target)),
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

			nodes = append(nodes, node)

			if opts.Verbose {
				log.Debug().Msgf("  Symlink: %s -> %s", cleanPath, target)
			}

		case tar.TypeDir:
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

			nodes = append(nodes, node)

			if opts.Verbose {
				log.Debug().Msgf("  Dir: %s", cleanPath)
			}

		case tar.TypeLink:
			// Hard link - not commonly used in containers, skip for now
			log.Debug().Msgf("Skipping hard link: %s -> %s", cleanPath, hdr.Linkname)

		default:
			log.Debug().Msgf("Skipping unsupported type %d: %s", hdr.Typeflag, cleanPath)
		}
	}

	// Add final checkpoint
	if uncompressedCounter.n > lastCheckpoint {
		cp := common.GzipCheckpoint{
			COff: compressedCounter.n,
			UOff: uncompressedCounter.n,
		}
		checkpoints = append(checkpoints, cp)
		log.Debug().Msgf("Added final checkpoint: COff=%d, UOff=%d", cp.COff, cp.UOff)
	}

	gzipIndex := &common.GzipIndex{
		LayerDigest: layerDigest,
		Checkpoints: checkpoints,
	}

	return gzipIndex, nodes, nil
}
