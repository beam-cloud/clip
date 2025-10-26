package clip

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
	"golang.org/x/sys/unix"

	common "github.com/beam-cloud/clip/pkg/common"

	"github.com/karrick/godirwalk"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&common.ClipNode{})
	gob.Register(&common.StorageInfoWrapper{})
	gob.Register(&common.S3StorageInfo{})
	gob.Register(&common.OCIStorageInfo{})
	gob.Register(&common.OCILayoutStorageInfo{})
	gob.Register(&common.RemoteRef{})
	gob.Register(&common.GzipIndex{})
	gob.Register(&common.GzipCheckpoint{})
	gob.Register(&common.ZstdIndex{})
	gob.Register(&common.ZstdFrame{})
}

type ClipArchiverOptions struct {
	Verbose     bool
	Compress    bool
	ArchivePath string
	SourcePath  string
	OutputFile  string
	OutputPath  string
}

type ClipArchiver struct {
}

func NewClipArchiver() *ClipArchiver {
	return &ClipArchiver{}
}

func (ca *ClipArchiver) newIndex() *btree.BTree {
	compare := func(a, b interface{}) bool {
		return a.(*common.ClipNode).Path < b.(*common.ClipNode).Path
	}
	return btree.New(compare)
}

// InodeGenerator generates unique inodes for each ClipNode
type InodeGenerator struct {
	current uint64
}

func (ig *InodeGenerator) Next() uint64 {
	ig.current++
	return ig.current
}

// populateIndex creates a representation of the filesystem/folder being archived
func (ca *ClipArchiver) populateIndex(index *btree.BTree, sourcePath string) error {
	root := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Mode: uint32(os.ModeDir | 0755),
		},
	}
	index.Set(root)

	inodeGen := &InodeGenerator{current: 0}
	inodeMap := make(map[string]uint64)

	err := godirwalk.Walk(sourcePath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			// Get stat info first to check file type
			var stat unix.Stat_t
			var err error
			if de.IsSymlink() {
				err = unix.Lstat(path, &stat)
			} else {
				err = unix.Stat(path, &stat)
			}
			if err != nil {
				return err
			}

			// Skip device files
			if (stat.Mode&unix.S_IFMT) == unix.S_IFCHR || (stat.Mode&unix.S_IFMT) == unix.S_IFBLK {
				log.Info().Msgf("skipping device file: %s", path)
				return nil
			}

			var target string = ""
			var nodeType common.ClipNodeType

			if de.IsDir() {
				nodeType = common.DirNode
			} else if de.IsSymlink() {
				_target, err := os.Readlink(path)
				if err != nil {
					return fmt.Errorf("error reading symlink target %s: %v", path, err)
				}
				target = _target
				nodeType = common.SymLinkNode
			} else {
				nodeType = common.FileNode
			}

			var contentHash = ""
			if nodeType == common.FileNode {
				fileContent, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("failed to read file contents for hashing: %w", err)
				}

				hash := sha256.Sum256(fileContent)
				contentHash = hex.EncodeToString(hash[:])
			}

			// Determine the file mode and type
			mode := uint32(stat.Mode & 0777) // preserve permission bits only
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				mode |= syscall.S_IFDIR
			case unix.S_IFLNK:
				mode |= syscall.S_IFLNK
			case unix.S_IFREG:
				mode |= syscall.S_IFREG
			default:
				// Handle other types if needed
				mode |= syscall.S_IFREG
			}
			// Assign a unique inode
			var inode uint64
			if existingInode, exists := inodeMap[path]; exists {
				inode = existingInode
			} else {
				inode = inodeGen.Next()
				inodeMap[path] = inode
			}

			attr := fuse.Attr{
				Ino:       inode,
				Size:      uint64(stat.Size),
				Blocks:    uint64(stat.Blocks),
				Atime:     uint64(stat.Atim.Sec),
				Atimensec: uint32(stat.Atim.Nsec),
				Mtime:     uint64(stat.Mtim.Sec),
				Mtimensec: uint32(stat.Mtim.Nsec),
				Ctime:     uint64(stat.Ctim.Sec),
				Ctimensec: uint32(stat.Ctim.Nsec),
				Mode:      mode,
				Nlink:     uint32(stat.Nlink),
				Owner: fuse.Owner{
					Uid: stat.Uid,
					Gid: stat.Gid,
				},
			}

			pathWithPrefix := filepath.Join("/", strings.TrimPrefix(path, sourcePath))
			index.Set(&common.ClipNode{Path: pathWithPrefix, NodeType: nodeType, Attr: attr, Target: target, ContentHash: contentHash})

			return nil
		},
		Unsorted: false,
	})

	return err
}

func (ca *ClipArchiver) Create(opts ClipArchiverOptions) error {
	outFile, err := os.Create(opts.OutputFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// Create a new index for the archive
	index := ca.newIndex()

	err = ca.populateIndex(index, opts.SourcePath)
	if err != nil {
		return err
	}

	// Prepare and write placeholder for the header
	var storageType [12]byte
	copy(storageType[:], []byte(""))
	header := common.ClipArchiveHeader{
		ClipFileFormatVersion: common.ClipFileFormatVersion,
		IndexLength:           0,
		StorageInfoLength:     0,
		StorageInfoPos:        0,
		StorageInfoType:       storageType,
	}
	copy(header.StartBytes[:], common.ClipFileStartBytes)

	headerPos, err := outFile.Seek(0, io.SeekCurrent) // Get current position
	if err != nil {
		return err
	}

	// Write placeholder bytes for the header
	if _, err := outFile.Write(make([]byte, common.ClipHeaderLength)); err != nil {
		return err
	}

	// Write data blocks
	var initialOffset int64 = int64(common.ClipHeaderLength)
	err = ca.writeBlocks(index, opts.SourcePath, outFile, initialOffset, opts)
	if err != nil {
		return err
	}

	// Write the actual index data
	indexPos, err := outFile.Seek(0, io.SeekCurrent) // Get current position
	if err != nil {
		return err
	}

	indexBytes, err := ca.EncodeIndex(index)
	if err != nil {
		return err
	}

	if _, err := outFile.Write(indexBytes); err != nil {
		return err
	}

	// Update the header with the correct index size and position
	header.IndexLength = int64(len(indexBytes))
	header.IndexPos = indexPos

	headerBytes, err := ca.EncodeHeader(&header)
	if err != nil {
		return err
	}

	_, err = outFile.Seek(headerPos, os.SEEK_SET) // Go back to header position
	if err != nil {
		return err
	}

	if _, err := outFile.Write(headerBytes); err != nil {
		return err
	}

	return nil
}

func (ca *ClipArchiver) CreateRemoteArchive(storageInfo common.ClipStorageInfo, metadata *common.ClipArchiveMetadata, outputFile string) error {
	outFile, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// Prepare and write placeholder for the header
	var storageType [12]byte
	copy(storageType[:], []byte(storageInfo.Type()))

	header := common.ClipArchiveHeader{
		ClipFileFormatVersion: common.ClipFileFormatVersion,
		IndexLength:           0,
		StorageInfoLength:     0,
		StorageInfoPos:        0,
		StorageInfoType:       storageType,
	}
	copy(header.StartBytes[:], common.ClipFileStartBytes)

	headerPos, err := outFile.Seek(0, io.SeekCurrent) // Get current position
	if err != nil {
		return err
	}

	// Write placeholder bytes for the header
	if _, err := outFile.Write(make([]byte, common.ClipHeaderLength)); err != nil {
		return err
	}

	// Write the actual index data
	indexPos, err := outFile.Seek(0, io.SeekCurrent) // Get current position
	if err != nil {
		return err
	}

	indexBytes, err := ca.EncodeIndex(metadata.Index)
	if err != nil {
		return err
	}

	if _, err := outFile.Write(indexBytes); err != nil {
		return err
	}

	// Update the header with the correct index size and position
	header.IndexLength = int64(len(indexBytes))
	header.IndexPos = indexPos

	// Encode storage info
	header.StorageInfoPos = header.IndexPos + header.IndexLength

	storageInfoBytes, err := storageInfo.Encode()
	if err != nil {
		return err
	}

	// Wrap encoded storage info in a StorageInfoWrapper
	wrapper := common.StorageInfoWrapper{
		Type: storageInfo.Type(),
		Data: storageInfoBytes,
	}

	// Encode the wrapper
	var buf bytes.Buffer
	wrapperEnc := gob.NewEncoder(&buf)
	if err := wrapperEnc.Encode(wrapper); err != nil {
		return err
	}

	wrapperBytes := buf.Bytes()

	// Write storage info at the end of the file
	header.StorageInfoLength = int64(len(wrapperBytes))
	if _, err := outFile.Write(wrapperBytes); err != nil {
		return err
	}

	// Finally, encode and write the header
	headerBytes, err := ca.EncodeHeader(&header)
	if err != nil {
		return err
	}

	_, err = outFile.Seek(headerPos, os.SEEK_SET) // Go back to header position
	if err != nil {
		return err
	}

	if _, err := outFile.Write(headerBytes); err != nil {
		return err
	}

	return nil
}

func (ca *ClipArchiver) ExtractMetadata(archivePath string) (*common.ClipArchiveMetadata, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Read and decode the header
	headerBytes := make([]byte, common.ClipHeaderLength)
	if _, err := io.ReadFull(file, headerBytes); err != nil {
		return nil, common.ErrFileHeaderMismatch
	}

	// Decode the header
	header, err := ca.DecodeHeader(headerBytes)
	if err != nil {
		return nil, common.ErrFileHeaderMismatch
	}

	// Verify the header
	if !bytes.Equal(header.StartBytes[:], common.ClipFileStartBytes) || header.ClipFileFormatVersion != common.ClipFileFormatVersion {
		return nil, common.ErrFileHeaderMismatch
	}

	// Seek to the correct position for the index
	_, err = file.Seek(header.IndexPos, 0)
	if err != nil {
		return nil, fmt.Errorf("error seeking to index: %v", err)
	}

	// Read and decode the index
	indexBytes := make([]byte, header.IndexLength)
	if _, err := io.ReadFull(file, indexBytes); err != nil {
		return nil, fmt.Errorf("error reading index: %v", err)
	}

	indexReader := bytes.NewReader(indexBytes)
	indexDec := gob.NewDecoder(indexReader)

	var nodes []*common.ClipNode
	if err := indexDec.Decode(&nodes); err != nil {
		return nil, fmt.Errorf("error decoding index: %v", err)
	}

	index := ca.newIndex()
	for _, node := range nodes {
		index.Set(node)
	}

	var storageInfo common.ClipStorageInfo
	if header.StorageInfoLength > 0 {
		// Read and decode the storage info
		_, err = file.Seek(header.StorageInfoPos, 0)
		if err != nil {
			return nil, fmt.Errorf("error seeking to storage info: %v", err)
		}

		storageBytes := make([]byte, header.StorageInfoLength)
		if _, err := io.ReadFull(file, storageBytes); err != nil {
			return nil, fmt.Errorf("error reading storage info: %v", err)
		}

		storageReader := bytes.NewReader(storageBytes)
		storageDec := gob.NewDecoder(storageReader)

		var wrapper common.StorageInfoWrapper
		if err := storageDec.Decode(&wrapper); err != nil {
			return nil, fmt.Errorf("error decoding storage info: %v", err)
		}

		switch wrapper.Type {
		case string(common.StorageModeS3):
			var s3Info common.S3StorageInfo
			if err := gob.NewDecoder(bytes.NewReader(wrapper.Data)).Decode(&s3Info); err != nil {
				return nil, fmt.Errorf("error decoding s3 storage info: %v", err)
			}
			storageInfo = s3Info
		case "oci":
			var ociInfo common.OCIStorageInfo
			if err := gob.NewDecoder(bytes.NewReader(wrapper.Data)).Decode(&ociInfo); err != nil {
				return nil, fmt.Errorf("error decoding oci storage info: %v", err)
			}
			storageInfo = ociInfo
		case "oci-layout":
			var ociLayoutInfo common.OCILayoutStorageInfo
			if err := gob.NewDecoder(bytes.NewReader(wrapper.Data)).Decode(&ociLayoutInfo); err != nil {
				return nil, fmt.Errorf("error decoding oci layout storage info: %v", err)
			}
			storageInfo = ociLayoutInfo
		default:
			return nil, fmt.Errorf("unsupported storage info type: %s", wrapper.Type)
		}
	}

	return &common.ClipArchiveMetadata{
		Index:       index,
		Header:      *header,
		StorageInfo: storageInfo,
	}, nil
}

func (ca *ClipArchiver) Extract(opts ClipArchiverOptions) error {
	file, err := os.Open(opts.ArchivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	os.MkdirAll(opts.OutputPath, 0755)

	// Read and decode the header
	headerBytes := make([]byte, common.ClipHeaderLength)
	if _, err := io.ReadFull(file, headerBytes); err != nil {
		return common.ErrFileHeaderMismatch
	}

	// Decode the header
	header, err := ca.DecodeHeader(headerBytes)
	if err != nil {
		return common.ErrFileHeaderMismatch
	}

	// Verify the header
	if !bytes.Equal(header.StartBytes[:], common.ClipFileStartBytes) || header.ClipFileFormatVersion != common.ClipFileFormatVersion {
		return common.ErrFileHeaderMismatch
	}

	// Seek to the correct position for the index
	_, err = file.Seek(header.IndexPos, 0)
	if err != nil {
		return fmt.Errorf("error seeking to index: %v", err)
	}

	// Read and decode the index
	indexBytes := make([]byte, header.IndexLength)
	if _, err := io.ReadFull(file, indexBytes); err != nil {
		return fmt.Errorf("error reading index: %v", err)
	}

	indexReader := bytes.NewReader(indexBytes)
	indexDec := gob.NewDecoder(indexReader)

	var nodes []*common.ClipNode
	if err := indexDec.Decode(&nodes); err != nil {
		return fmt.Errorf("error decoding index: %v", err)
	}

	index := ca.newIndex()
	for _, node := range nodes {
		index.Set(node)
	}

	// Iterate over the index and extract every node
	index.Ascend(index.Min(), func(a interface{}) bool {
		node := a.(*common.ClipNode)

		if opts.Verbose {
			log.Info().Msgf("Extracting... %s", node.Path)
		}

		if node.NodeType == common.FileNode {
			// Seek to the position of the file in the archive
			_, err := file.Seek(node.DataPos, 0)
			if err != nil {
				log.Error().Msgf("error seeking to file %s: %v", node.Path, err)
				return false
			}

			// Open the output file
			outFile, err := os.Create(path.Join(opts.OutputPath, node.Path))
			if err != nil {
				log.Error().Msgf("error creating file %s: %v", node.Path, err)
				return false
			}
			defer outFile.Close()

			// Copy the data from the archive to the output file
			_, err = io.CopyN(outFile, file, node.DataLen)
			if err != nil {
				log.Error().Msgf("error extracting file %s: %v", node.Path, err)
				return false
			}

		} else if node.NodeType == common.DirNode {
			os.MkdirAll(path.Join(opts.OutputPath, node.Path), fs.FileMode(node.Attr.Mode))
		} else if node.NodeType == common.SymLinkNode {
			os.Symlink(node.Target, path.Join(opts.OutputPath, node.Path))
		}

		return true
	})

	return nil
}

func (ca *ClipArchiver) writeBlocks(index *btree.BTree, sourcePath string, outFile *os.File, offset int64, opts ClipArchiverOptions) error {
	writer := bufio.NewWriterSize(outFile, 512*1024)
	defer writer.Flush() // Ensure all data gets written when we're done

	var pos int64 = offset

	// Push specific directories towards the front of the archive
	priorityDirs := []string{
		path.Join(sourcePath, "/rootfs/usr/lib"),
		path.Join(sourcePath, "/rootfs/usr/bin"),
		path.Join(sourcePath, "/rootfs/usr/local/lib/python3.7/dist-packages"),
		path.Join(sourcePath, "/rootfs/usr/local/lib/python3.8/dist-packages"),
		path.Join(sourcePath, "/rootfs/usr/local/lib/python3.9/dist-packages"),
		path.Join(sourcePath, "/rootfs/usr/local/lib/python3.10/dist-packages"),
	}

	// Create slices for priority nodes and other nodes
	var priorityNodes []*common.ClipNode
	var otherNodes []*common.ClipNode

	// Separate nodes into priority and other
	index.Ascend(index.Min(), func(a interface{}) bool {
		node := a.(*common.ClipNode)
		isPriority := false

		nodeFullPath := path.Join(sourcePath, node.Path) // Adding sourcePath to the node path
		for _, dir := range priorityDirs {
			if strings.HasPrefix(nodeFullPath, dir) {
				isPriority = true
				break
			}
		}

		if isPriority {
			priorityNodes = append(priorityNodes, node)
		} else {
			otherNodes = append(otherNodes, node)
		}
		return true
	})

	// Process priority nodes first
	for _, node := range priorityNodes {
		if node.NodeType == common.FileNode {
			if !ca.processNode(node, writer, sourcePath, &pos, opts) {
				return fmt.Errorf("error processing priority node %s", node.Path)
			}
		}
	}

	// Process other nodes
	for _, node := range otherNodes {
		if node.NodeType == common.FileNode {
			if !ca.processNode(node, writer, sourcePath, &pos, opts) {
				return fmt.Errorf("error processing other node %s", node.Path)
			}
		}
	}

	return nil
}

func (ca *ClipArchiver) processNode(node *common.ClipNode, writer *bufio.Writer, sourcePath string, pos *int64, opts ClipArchiverOptions) bool {
	if opts.Verbose {
		log.Info().Msgf("Archiving... %s", node.Path)
	}

	f, err := os.Open(path.Join(sourcePath, node.Path))
	if err != nil {
		log.Error().Msgf("error opening source file %s: %v", node.Path, err)
		return false
	}
	defer f.Close()

	// Initialize CRC64 table and hash
	table := crc64.MakeTable(crc64.ISO)
	hash := crc64.New(table)

	blockType := common.BlockTypeFile

	// Write block type
	if err := binary.Write(writer, binary.LittleEndian, blockType); err != nil {
		log.Error().Msgf("error writing block type: %v", err)
		return false
	}

	// Increment position to account for block type
	*pos += 1

	// Update data position
	node.DataPos = *pos

	// Create a multi-writer that writes to both the checksum and the writer
	multi := io.MultiWriter(hash, writer)

	// Use io.Copy to simultaneously write the file to the output and update the checksum
	copied, err := io.Copy(multi, f)
	if err != nil {
		log.Error().Msgf("error copying file %s: %v", node.Path, err)
		return false
	}

	// Compute final CRC64 checksum
	checksum := hash.Sum(nil)

	// Write checksum to output file
	if _, err := writer.Write(checksum); err != nil {
		log.Error().Msgf("error writing checksum: %v", err)
		return false
	}

	// Increment position to account for checksum
	*pos += common.ClipChecksumLength

	// Update node with data length
	node.DataLen = copied

	*pos += copied

	return true
}

func (ca *ClipArchiver) EncodeHeader(header *common.ClipArchiveHeader) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (ca *ClipArchiver) DecodeHeader(headerBytes []byte) (*common.ClipArchiveHeader, error) {
	header := new(common.ClipArchiveHeader)
	buf := bytes.NewBuffer(headerBytes)
	if err := binary.Read(buf, binary.LittleEndian, header); err != nil {
		return nil, err
	}
	return header, nil
}

func (ca *ClipArchiver) EncodeIndex(index *btree.BTree) ([]byte, error) {
	var nodes []*common.ClipNode
	index.Ascend(index.Min(), func(a interface{}) bool {
		nodes = append(nodes, a.(*common.ClipNode))
		return true
	})

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(nodes); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Helper structures for OCI indexing
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	k, err := cr.r.Read(p)
	cr.n += int64(k)
	return k, err
}

type gzipIndexBuilder struct {
	checkpoints     []common.GzipCheckpoint
	checkpointMiB   int64
	lastCheckpointU int64
}

func newGzipIndexBuilder(checkpointMiB int64) *gzipIndexBuilder {
	if checkpointMiB <= 0 {
		checkpointMiB = 2 // default 2 MiB
	}
	return &gzipIndexBuilder{
		checkpoints:   make([]common.GzipCheckpoint, 0),
		checkpointMiB: checkpointMiB,
	}
}

func (gib *gzipIndexBuilder) maybeAddCheckpoint(cOff, uOff int64) {
	checkpointBytes := gib.checkpointMiB * 1024 * 1024
	if uOff-gib.lastCheckpointU >= checkpointBytes || len(gib.checkpoints) == 0 {
		gib.checkpoints = append(gib.checkpoints, common.GzipCheckpoint{
			COff: cOff,
			UOff: uOff,
		})
		gib.lastCheckpointU = uOff
	}
}

func (gib *gzipIndexBuilder) build(layerDigest string) *common.GzipIndex {
	return &common.GzipIndex{
		LayerDigest: layerDigest,
		Checkpoints: gib.checkpoints,
	}
}

// nearestCheckpoint finds the largest checkpoint UOff <= wantU
func nearestCheckpoint(checkpoints []common.GzipCheckpoint, wantU int64) (cOff, uOff int64) {
	if len(checkpoints) == 0 {
		return 0, 0
	}
	
	// Binary search for largest UOff <= wantU
	i := sort.Search(len(checkpoints), func(i int) bool {
		return checkpoints[i].UOff > wantU
	}) - 1
	
	if i < 0 {
		i = 0
	}
	
	return checkpoints[i].COff, checkpoints[i].UOff
}

// isWhiteout checks if a tar entry is a whiteout file
func isWhiteout(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(base, ".wh.")
}

// applyWhiteout applies OCI whiteout semantics to the index
func applyWhiteout(index *btree.BTree, hdrName string) {
	base := path.Base(hdrName)
	dir := path.Dir(hdrName)
	
	if base == ".wh..wh..opq" {
		// Remove all entries under dir from lower layers
		prefix := "/" + strings.TrimPrefix(dir, "./") + "/"
		if prefix == "//" {
			prefix = "/"
		}
		
		// Collect items to delete
		var toDelete []*common.ClipNode
		index.Ascend(&common.ClipNode{Path: prefix}, func(item interface{}) bool {
			node := item.(*common.ClipNode)
			if strings.HasPrefix(node.Path, prefix) && node.Path != prefix {
				toDelete = append(toDelete, node)
			}
			return true
		})
		
		// Delete collected items
		for _, node := range toDelete {
			index.Delete(node)
		}
		return
	}
	
	if strings.HasPrefix(base, ".wh.") {
		victim := path.Join(dir, strings.TrimPrefix(base, ".wh."))
		victimPath := "/" + strings.TrimPrefix(victim, "./")
		
		// Remove the victim file/dir
		if existing := index.Get(&common.ClipNode{Path: victimPath}); existing != nil {
			index.Delete(existing)
		}
		
		// Remove anything underneath if it's a directory
		victimPrefix := victimPath + "/"
		var toDelete []*common.ClipNode
		index.Ascend(&common.ClipNode{Path: victimPrefix}, func(item interface{}) bool {
			node := item.(*common.ClipNode)
			if strings.HasPrefix(node.Path, victimPrefix) {
				toDelete = append(toDelete, node)
			}
			return true
		})
		
		for _, node := range toDelete {
			index.Delete(node)
		}
	}
}

// setOrMerge adds or updates a node in the index (overlay semantics)
func setOrMerge(index *btree.BTree, node *common.ClipNode) {
	// In overlay semantics, upper layers override lower layers
	index.Set(node)
}

// IndexOCIImage creates an index from an OCI image without extracting layer data
func (ca *ClipArchiver) IndexOCIImage(ctx context.Context, ref string) (
	index *btree.BTree,
	layerDigests []string,
	gzipIdx map[string]*common.GzipIndex,
	zstdIdx map[string]*common.ZstdIndex,
	err error,
) {
	log.Info().Msgf("indexing OCI image: %s", ref)
	
	// Parse image reference
	imageRef, err := name.ParseReference(ref)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to parse image reference: %w", err)
	}
	
	// Fetch image descriptor
	img, err := remote.Image(imageRef)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to fetch image: %w", err)
	}
	
	// Get manifest to extract layer information
	manifest, err := img.Manifest()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get manifest: %w", err)
	}
	
	// Initialize return values
	index = ca.newIndex()
	layerDigests = make([]string, len(manifest.Layers))
	gzipIdx = make(map[string]*common.GzipIndex)
	zstdIdx = make(map[string]*common.ZstdIndex)
	
	// Add root directory
	rootNode := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Mode: uint32(os.ModeDir | 0755),
			Ino:  1,
		},
	}
	index.Set(rootNode)
	
	// Process layers in order
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get layers: %w", err)
	}
	
	for i, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to get layer digest: %w", err)
		}
		
		layerDigest := digest.String()
		layerDigests[i] = layerDigest
		
		log.Info().Msgf("processing layer %d/%d: %s", i+1, len(layers), layerDigest)
		
		// Get compressed layer stream
		compressedRC, err := layer.Compressed()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to get compressed layer: %w", err)
		}
		defer compressedRC.Close()
		
		// Check media type to determine compression
		mediaType, err := layer.MediaType()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to get layer media type: %w", err)
		}
		
		switch mediaType {
		case "application/vnd.docker.image.rootfs.diff.tar.gzip",
			 "application/vnd.oci.image.layer.v1.tar+gzip":
			// Process gzip layer
			gzipIndex, err := ca.indexGzipLayer(compressedRC, layerDigest, index)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("failed to index gzip layer %s: %w", layerDigest, err)
			}
			gzipIdx[layerDigest] = gzipIndex
			
		case "application/vnd.oci.image.layer.v1.tar+zstd":
			// TODO: Implement zstd indexing (P1)
			return nil, nil, nil, nil, fmt.Errorf("zstd layers not yet supported")
			
		default:
			return nil, nil, nil, nil, fmt.Errorf("unsupported layer media type: %s", mediaType)
		}
	}
	
	log.Info().Msgf("successfully indexed OCI image with %d layers", len(layerDigests))
	return index, layerDigests, gzipIdx, zstdIdx, nil
}

// indexGzipLayer processes a single gzip-compressed layer
func (ca *ClipArchiver) indexGzipLayer(compressedRC io.ReadCloser, layerDigest string, index *btree.BTree) (*common.GzipIndex, error) {
	defer compressedRC.Close()
	
	// Create gzip index builder with 2 MiB checkpoints
	gzipBuilder := newGzipIndexBuilder(2)
	
	// Create a TeeReader to track compressed bytes while decompressing
	compressedCounter := &countingReader{r: compressedRC}
	
	// Create gzip reader
	gzr, err := gzip.NewReader(compressedCounter)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()
	
	// Create counting reader for uncompressed bytes
	uncompressedCounter := &countingReader{r: gzr}
	
	// Create tar reader
	tr := tar.NewReader(uncompressedCounter)
	
	// Process tar entries
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar header: %w", err)
		}
		
		// Record checkpoint periodically
		gzipBuilder.maybeAddCheckpoint(compressedCounter.n, uncompressedCounter.n)
		
		// Handle whiteouts
		if isWhiteout(hdr.Name) {
			applyWhiteout(index, hdr.Name)
			continue
		}
		
		// Convert tar header to ClipNode
		node, err := ca.tarHeaderToClipNode(hdr, layerDigest, uncompressedCounter.n)
		if err != nil {
			return nil, fmt.Errorf("failed to convert tar header: %w", err)
		}
		
		if node != nil {
			// For regular files, skip the data but track the position
			if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
				dataStart := uncompressedCounter.n
				
				// Skip file data by copying to discard
				if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
					return nil, fmt.Errorf("failed to skip file data: %w", err)
				}
				
				// Update node with remote reference
				node.Remote = &common.RemoteRef{
					LayerDigest: layerDigest,
					UOffset:     dataStart,
					ULength:     hdr.Size,
				}
			}
			
			// Add node to index (overlay semantics)
			setOrMerge(index, node)
		}
	}
	
	// Build final gzip index
	return gzipBuilder.build(layerDigest), nil
}

// tarHeaderToClipNode converts a tar header to a ClipNode
func (ca *ClipArchiver) tarHeaderToClipNode(hdr *tar.Header, layerDigest string, currentOffset int64) (*common.ClipNode, error) {
	// Clean path
	cleanPath := "/" + strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/")
	if cleanPath == "/" && hdr.Name != "." && hdr.Name != "./" {
		cleanPath = "/" + strings.TrimPrefix(hdr.Name, "./")
	}
	
	// Convert tar mode to fuse mode
	var mode uint32
	switch hdr.Typeflag {
	case tar.TypeDir:
		mode = uint32(hdr.Mode) | syscall.S_IFDIR
	case tar.TypeReg, tar.TypeRegA:
		mode = uint32(hdr.Mode) | syscall.S_IFREG
	case tar.TypeSymlink:
		mode = uint32(hdr.Mode) | syscall.S_IFLNK
	case tar.TypeLink:
		// Hard links - for now treat as regular files
		mode = uint32(hdr.Mode) | syscall.S_IFREG
	default:
		// Skip other types (char dev, block dev, etc.)
		return nil, nil
	}
	
	// Determine node type
	var nodeType common.ClipNodeType
	var target string
	
	switch hdr.Typeflag {
	case tar.TypeDir:
		nodeType = common.DirNode
	case tar.TypeReg, tar.TypeRegA, tar.TypeLink:
		nodeType = common.FileNode
	case tar.TypeSymlink:
		nodeType = common.SymLinkNode
		target = hdr.Linkname
	default:
		return nil, nil
	}
	
	// Create fuse attributes
	attr := fuse.Attr{
		Ino:   0, // Will be set later based on path hash
		Size:  uint64(hdr.Size),
		Mode:  mode,
		Nlink: 1,
		Owner: fuse.Owner{
			Uid: uint32(hdr.Uid),
			Gid: uint32(hdr.Gid),
		},
		Atime: uint64(hdr.AccessTime.Unix()),
		Mtime: uint64(hdr.ModTime.Unix()),
		Ctime: uint64(hdr.ChangeTime.Unix()),
	}
	
	// Generate stable inode based on layer digest and path
	attr.Ino = ca.generateInode(layerDigest, cleanPath)
	
	return &common.ClipNode{
		Path:     cleanPath,
		NodeType: nodeType,
		Attr:     attr,
		Target:   target,
		// ContentHash will be computed if needed
		// DataPos/DataLen are legacy fields, not used for OCI
		// Remote will be set by caller for regular files
	}, nil
}

// generateInode creates a stable inode number from layer digest and path
func (ca *ClipArchiver) generateInode(layerDigest, path string) uint64 {
	h := sha256.New()
	h.Write([]byte(layerDigest))
	h.Write([]byte(path))
	hash := h.Sum(nil)
	
	// Use first 8 bytes as inode, ensure it's not 0
	inode := binary.BigEndian.Uint64(hash[:8])
	if inode == 0 {
		inode = 1
	}
	return inode
}

// CreateFromOCI creates a metadata-only .clip file from an OCI image reference
func (ca *ClipArchiver) CreateFromOCI(
	ctx context.Context,
	imageRef, clipOut string,
	registryURL, authConfigPath string,
) error {
	log.Info().Msgf("creating clip from OCI image: %s -> %s", imageRef, clipOut)
	
	// Index the OCI image
	index, layerDigests, gzipIdx, zstdIdx, err := ca.IndexOCIImage(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("failed to index OCI image: %w", err)
	}
	
	// Parse image reference to extract repository info
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("failed to parse image reference: %w", err)
	}
	
	// Extract registry and repository
	if registryURL == "" {
		registryURL = ref.Context().RegistryStr()
	}
	repository := ref.Context().RepositoryStr()
	
	// Create OCI storage info
	storageInfo := &common.OCIStorageInfo{
		RegistryURL:        registryURL,
		Repository:         repository,
		Layers:             layerDigests,
		GzipIdxByLayer:     gzipIdx,
		ZstdIdxByLayer:     zstdIdx,
		AuthConfigPath:     authConfigPath,
	}
	
	// Create metadata
	metadata := &common.ClipArchiveMetadata{
		Header: common.ClipArchiveHeader{
			ClipFileFormatVersion: common.ClipFileFormatVersion,
		},
		Index:       index,
		StorageInfo: storageInfo,
	}
	
	// Create the remote archive (metadata-only)
	err = ca.CreateRemoteArchive(storageInfo, metadata, clipOut)
	if err != nil {
		return fmt.Errorf("failed to create remote archive: %w", err)
	}
	
	log.Info().Msgf("successfully created OCI clip file: %s", clipOut)
	return nil
}

// CreateFromOCILayout creates a metadata-only .clip file from a local OCI layout directory
func (ca *ClipArchiver) CreateFromOCILayout(
	ctx context.Context,
	ociLayoutPath, tag, clipOut string,
) error {
	log.Info().Msgf("creating clip from OCI layout: %s:%s -> %s", ociLayoutPath, tag, clipOut)
	
	// Index the local OCI layout
	index, layerDigests, gzipIdx, zstdIdx, err := ca.IndexOCILayout(ctx, ociLayoutPath, tag)
	if err != nil {
		return fmt.Errorf("failed to index OCI layout: %w", err)
	}
	
	// Create local OCI storage info (no registry URL needed)
	storageInfo := &common.OCILayoutStorageInfo{
		LayoutPath:         ociLayoutPath,
		Tag:               tag,
		Layers:            layerDigests,
		GzipIdxByLayer:    gzipIdx,
		ZstdIdxByLayer:    zstdIdx,
	}
	
	// Create metadata
	metadata := &common.ClipArchiveMetadata{
		Header: common.ClipArchiveHeader{
			ClipFileFormatVersion: common.ClipFileFormatVersion,
		},
		Index:       index,
		StorageInfo: storageInfo,
	}
	
	// Create the remote archive (metadata-only)
	err = ca.CreateRemoteArchive(storageInfo, metadata, clipOut)
	if err != nil {
		return fmt.Errorf("failed to create remote archive: %w", err)
	}
	
	log.Info().Msgf("successfully created OCI layout clip file: %s", clipOut)
	return nil
}

// IndexOCILayout creates an index from a local OCI layout directory
func (ca *ClipArchiver) IndexOCILayout(ctx context.Context, layoutPath, tag string) (
	index *btree.BTree,
	layerDigests []string,
	gzipIdx map[string]*common.GzipIndex,
	zstdIdx map[string]*common.ZstdIndex,
	err error,
) {
	log.Info().Msgf("indexing OCI layout: %s:%s", layoutPath, tag)
	
	// Read index.json to find the manifest
	indexPath := filepath.Join(layoutPath, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to read index.json: %w", err)
	}
	
	var ociIndex struct {
		Manifests []struct {
			Digest    string            `json:"digest"`
			MediaType string            `json:"mediaType"`
			Annotations map[string]string `json:"annotations,omitempty"`
		} `json:"manifests"`
	}
	
	if err := json.Unmarshal(indexData, &ociIndex); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to parse index.json: %w", err)
	}
	
	// Find manifest for the specified tag
	var manifestDigest string
	for _, manifest := range ociIndex.Manifests {
		if refTag, ok := manifest.Annotations["org.opencontainers.image.ref.name"]; ok && refTag == tag {
			manifestDigest = manifest.Digest
			break
		}
	}
	
	if manifestDigest == "" && len(ociIndex.Manifests) > 0 {
		// If no tag match, use the first manifest
		manifestDigest = ociIndex.Manifests[0].Digest
		log.Warn().Msgf("tag %s not found, using first manifest: %s", tag, manifestDigest)
	}
	
	if manifestDigest == "" {
		return nil, nil, nil, nil, fmt.Errorf("no manifest found for tag %s", tag)
	}
	
	// Read the manifest
	manifestPath := filepath.Join(layoutPath, "blobs", strings.ReplaceAll(manifestDigest, ":", "/"))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to read manifest %s: %w", manifestDigest, err)
	}
	
	var manifest struct {
		Layers []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to parse manifest: %w", err)
	}
	
	// Initialize return values
	index = ca.newIndex()
	layerDigests = make([]string, len(manifest.Layers))
	gzipIdx = make(map[string]*common.GzipIndex)
	zstdIdx = make(map[string]*common.ZstdIndex)
	
	// Add root directory
	rootNode := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Mode: uint32(os.ModeDir | 0755),
			Ino:  1,
		},
	}
	index.Set(rootNode)
	
	// Process layers in order
	for i, layer := range manifest.Layers {
		layerDigest := layer.Digest
		layerDigests[i] = layerDigest
		
		log.Info().Msgf("processing layer %d/%d: %s", i+1, len(manifest.Layers), layerDigest)
		
		// Open layer blob file
		blobPath := filepath.Join(layoutPath, "blobs", strings.ReplaceAll(layerDigest, ":", "/"))
		blobFile, err := os.Open(blobPath)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to open layer blob %s: %w", layerDigest, err)
		}
		defer blobFile.Close()
		
		// Check media type to determine compression
		switch layer.MediaType {
		case "application/vnd.docker.image.rootfs.diff.tar.gzip",
			 "application/vnd.oci.image.layer.v1.tar+gzip":
			// Process gzip layer
			gzipIndex, err := ca.indexGzipLayerFromFile(blobFile, layerDigest, index)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("failed to index gzip layer %s: %w", layerDigest, err)
			}
			gzipIdx[layerDigest] = gzipIndex
			
		case "application/vnd.oci.image.layer.v1.tar+zstd":
			// TODO: Implement zstd indexing (P1)
			return nil, nil, nil, nil, fmt.Errorf("zstd layers not yet supported")
			
		default:
			return nil, nil, nil, nil, fmt.Errorf("unsupported layer media type: %s", layer.MediaType)
		}
	}
	
	log.Info().Msgf("successfully indexed OCI layout with %d layers", len(layerDigests))
	return index, layerDigests, gzipIdx, zstdIdx, nil
}

// indexGzipLayerFromFile processes a single gzip-compressed layer from a file
func (ca *ClipArchiver) indexGzipLayerFromFile(file *os.File, layerDigest string, index *btree.BTree) (*common.GzipIndex, error) {
	// Create gzip index builder with 2 MiB checkpoints
	gzipBuilder := newGzipIndexBuilder(2)
	
	// Create a TeeReader to track compressed bytes while decompressing
	compressedCounter := &countingReader{r: file}
	
	// Create gzip reader
	gzr, err := gzip.NewReader(compressedCounter)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()
	
	// Create counting reader for uncompressed bytes
	uncompressedCounter := &countingReader{r: gzr}
	
	// Create tar reader
	tr := tar.NewReader(uncompressedCounter)
	
	// Process tar entries
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar header: %w", err)
		}
		
		// Record checkpoint periodically
		gzipBuilder.maybeAddCheckpoint(compressedCounter.n, uncompressedCounter.n)
		
		// Handle whiteouts
		if isWhiteout(hdr.Name) {
			applyWhiteout(index, hdr.Name)
			continue
		}
		
		// Convert tar header to ClipNode
		node, err := ca.tarHeaderToClipNode(hdr, layerDigest, uncompressedCounter.n)
		if err != nil {
			return nil, fmt.Errorf("failed to convert tar header: %w", err)
		}
		
		if node != nil {
			// For regular files, skip the data but track the position
			if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
				dataStart := uncompressedCounter.n
				
				// Skip file data by copying to discard
				if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
					return nil, fmt.Errorf("failed to skip file data: %w", err)
				}
				
				// Update node with remote reference
				node.Remote = &common.RemoteRef{
					LayerDigest: layerDigest,
					UOffset:     dataStart,
					ULength:     hdr.Size,
				}
			}
			
			// Add node to index (overlay semantics)
			setOrMerge(index, node)
		}
	}
	
	// Build final gzip index
	return gzipBuilder.build(layerDigest), nil
}
