package clip

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"hash/crc64"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/klauspost/compress/zstd"
	log "github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	common "github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"

	"github.com/karrick/godirwalk"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&common.ClipNode{})
	gob.Register(&common.StorageInfoWrapper{})
	gob.Register(&common.S3StorageInfo{})
	gob.Register(&common.OCIStorageInfo{})
	gob.Register(&common.RemoteRef{})
	gob.Register(&common.GzipCheckpoint{})
	gob.Register(&common.GzipIndex{})
	gob.Register(&common.ZstdFrame{})
	gob.Register(&common.ZstdIndex{})
}

type ClipArchiverOptions struct {
	Compress     bool
	ArchivePath  string
	SourcePath   string
	OutputFile   string
	OutputPath   string
	ContentCache storage.ContentCache
}

type ClipArchiver struct {
}

const (
	batchedMetadataVersion      uint8 = 2
	batchedMetadataMagic              = "CLIPBIN2"
	batchedMetadataHeaderLength       = int64(len(batchedMetadataMagic) + sha256.Size + 8 + 4)
	metadataBatchSize                 = 32768
	metadataBatchMaxBytes             = 64 << 20
	metadataBatchMaxFrames            = 65536
)

func NewClipArchiver() *ClipArchiver {
	return &ClipArchiver{}
}

func (ca *ClipArchiver) newIndex() *btree.BTree {
	compare := func(a, b interface{}) bool {
		return a.(*common.ClipNode).Path < b.(*common.ClipNode).Path
	}
	// Wider nodes reduce serial index construction work for large metadata archives.
	return btree.NewOptions(compare, btree.Options{Degree: 128})
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
	// Create root directory
	now := time.Now()
	rootTime, rootTimensec := fuseAttrTime(now)
	root := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Ino:       1,
			Size:      0,
			Blocks:    0,
			Atime:     rootTime,
			Atimensec: rootTimensec,
			Mtime:     rootTime,
			Mtimensec: rootTimensec,
			Ctime:     rootTime,
			Ctimensec: rootTimensec,
			Mode:      uint32(syscall.S_IFDIR | 0755),
			Nlink:     2, // Directories start with link count of 2 (. and ..)
			Owner: fuse.Owner{
				Uid: 0, // root
				Gid: 0, // root
			},
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

			atime, atimensec := fuseAttrTimespec(stat.Atim.Sec, stat.Atim.Nsec)
			mtime, mtimensec := fuseAttrTimespec(stat.Mtim.Sec, stat.Mtim.Nsec)
			ctime, ctimensec := fuseAttrTimespec(stat.Ctim.Sec, stat.Ctim.Nsec)
			attr := fuse.Attr{
				Ino:       inode,
				Size:      uint64(stat.Size),
				Blocks:    uint64(stat.Blocks),
				Atime:     atime,
				Atimensec: atimensec,
				Mtime:     mtime,
				Mtimensec: mtimensec,
				Ctime:     ctime,
				Ctimensec: ctimensec,
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
	if !bytes.Equal(header.StartBytes[:], common.ClipFileStartBytes) || (header.ClipFileFormatVersion != common.ClipFileFormatVersion && header.ClipFileFormatVersion != batchedMetadataVersion) {
		return nil, common.ErrFileHeaderMismatch
	}

	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if header.IndexPos < 0 || header.IndexLength < 0 || header.IndexPos > stat.Size() || header.IndexLength > stat.Size()-header.IndexPos {
		return nil, fmt.Errorf("error reading index: %w", io.ErrUnexpectedEOF)
	}
	metadata := &common.ClipArchiveMetadata{Index: ca.newIndex(), Header: *header}
	index := metadata.Index
	if header.ClipFileFormatVersion == batchedMetadataVersion {
		if err := ca.decodeIndexV2(file, metadata); err != nil {
			return nil, err
		}
	} else {
		var nodes []*common.ClipNode
		if err := gob.NewDecoder(io.NewSectionReader(file, header.IndexPos, header.IndexLength)).Decode(&nodes); err != nil {
			return nil, fmt.Errorf("error decoding index: %v", err)
		}
		for _, node := range nodes {
			index.Load(node)
		}
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
		case string(common.StorageModeOCI):
			var ociInfo common.OCIStorageInfo
			if err := gob.NewDecoder(bytes.NewReader(wrapper.Data)).Decode(&ociInfo); err != nil {
				return nil, fmt.Errorf("error decoding oci storage info: %v", err)
			}
			storageInfo = ociInfo
		default:
			return nil, fmt.Errorf("unsupported storage info type: %s", wrapper.Type)
		}
	}

	metadata.StorageInfo = storageInfo
	return metadata, nil
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
		log.Debug().Str("path", node.Path).Msg("Extracting")

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
	log.Debug().Str("path", node.Path).Msg("Archiving")

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

	copied, ok := ca.copyNodeContent(node, f, writer, hash, opts)
	if !ok {
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

func (ca *ClipArchiver) copyNodeContent(node *common.ClipNode, f *os.File, writer *bufio.Writer, hash io.Writer, opts ClipArchiverOptions) (int64, bool) {
	if opts.ContentCache == nil || node.ContentHash == "" {
		multi := io.MultiWriter(hash, writer)
		copied, err := io.Copy(multi, f)
		if err != nil {
			log.Error().Msgf("error copying file %s: %v", node.Path, err)
			return copied, false
		}
		return copied, true
	}

	chunks := make(chan []byte, 2)
	type storeResult struct {
		hash string
		err  error
	}
	storeDone := make(chan storeResult, 1)
	go func() {
		actualHash, err := opts.ContentCache.StoreContent(chunks, node.ContentHash, struct{ RoutingKey string }{RoutingKey: node.ContentHash})
		storeDone <- storeResult{hash: actualHash, err: err}
	}()

	buf := make([]byte, indexedLayerContentCacheChunkSize)
	var copied int64
	var copyOK bool
	var result *storeResult
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			data := buf[:n]
			if _, err := hash.Write(data); err != nil {
				log.Error().Err(err).Str("path", node.Path).Msg("error hashing file while archiving")
				break
			}
			if _, err := writer.Write(data); err != nil {
				log.Error().Err(err).Str("path", node.Path).Msg("error writing file to archive")
				break
			}
			chunk := make([]byte, n)
			copy(chunk, data)
			if chunks != nil {
				select {
				case chunks <- chunk:
				case r := <-storeDone:
					result = &r
					chunks = nil
					log.Warn().
						Err(r.err).
						Str("path", node.Path).
						Str("hash", node.ContentHash).
						Str("actual_hash", r.hash).
						Msg("file content cache store ended before archive copy completed")
				}
			}
			copied += int64(n)
		}
		if readErr == io.EOF {
			copyOK = true
			break
		}
		if readErr != nil {
			log.Error().Err(readErr).Str("path", node.Path).Msg("error reading file while archiving")
			break
		}
	}

	if chunks != nil {
		close(chunks)
	}
	if result == nil && chunks != nil {
		r := <-storeDone
		result = &r
	}
	if result != nil && (result.err != nil || result.hash != node.ContentHash) {
		log.Warn().
			Err(result.err).
			Str("path", node.Path).
			Str("hash", node.ContentHash).
			Str("actual_hash", result.hash).
			Msg("error warming file content cache")
	}

	return copied, copyOK
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

// Version 2 stores independent compressed batches with a fixed node field order.
func (ca *ClipArchiver) encodeIndexV2(index *btree.BTree, sourceHash []byte, sourceSize int64) ([]byte, error) {
	var nodes []*common.ClipNode
	index.Ascend(nil, func(item interface{}) bool { nodes = append(nodes, item.(*common.ClipNode)); return true })
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(4<<20), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	defer encoder.Close()
	var frames [][]byte
	for start := 0; start < len(nodes); {
		end := min(start+metadataBatchSize, len(nodes))
		data := encodeNodeBatch(nodes[start:end])
		for len(data) > metadataBatchMaxBytes {
			if end-start == 1 {
				return nil, fmt.Errorf("metadata node exceeds batch size limit")
			}
			end = start + (end-start)/2
			data = encodeNodeBatch(nodes[start:end])
		}
		if len(frames) == metadataBatchMaxFrames {
			return nil, fmt.Errorf("metadata exceeds batch count limit")
		}
		frames = append(frames, encoder.EncodeAll(data, nil))
		start = end
	}
	var out bytes.Buffer
	out.WriteString(batchedMetadataMagic)
	out.Write(sourceHash)
	binary.Write(&out, binary.LittleEndian, sourceSize)
	binary.Write(&out, binary.LittleEndian, uint32(len(frames)))
	for _, frame := range frames {
		binary.Write(&out, binary.LittleEndian, uint32(len(frame)))
	}
	for _, frame := range frames {
		out.Write(frame)
	}
	return out.Bytes(), nil
}

func (ca *ClipArchiver) decodeIndexV2(file *os.File, metadata *common.ClipArchiveMetadata) error {
	header, index := &metadata.Header, metadata.Index
	reader := io.NewSectionReader(file, header.IndexPos, header.IndexLength)
	var marker [len(batchedMetadataMagic)]byte
	if _, err := io.ReadFull(reader, marker[:]); err != nil {
		return err
	}
	if string(marker[:]) != batchedMetadataMagic {
		return fmt.Errorf("invalid v2 index marker")
	}
	var sourceHash [sha256.Size]byte
	if _, err := io.ReadFull(reader, sourceHash[:]); err != nil {
		return err
	}
	metadata.OriginalArchiveHash = hex.EncodeToString(sourceHash[:])
	if err := binary.Read(reader, binary.LittleEndian, &metadata.OriginalArchiveSize); err != nil {
		return err
	}
	if metadata.OriginalArchiveSize < common.ClipHeaderLength {
		return fmt.Errorf("invalid source archive size")
	}
	var count uint32
	if err := binary.Read(reader, binary.LittleEndian, &count); err != nil {
		return err
	}
	if count > metadataBatchMaxFrames || batchedMetadataHeaderLength+int64(count)*4 > header.IndexLength {
		return fmt.Errorf("invalid v2 index frame count")
	}
	lengths := make([]uint32, count)
	if err := binary.Read(reader, binary.LittleEndian, lengths); err != nil {
		return err
	}
	offsets := make([]int64, count)
	offset := batchedMetadataHeaderLength + int64(count)*4
	for i, length := range lengths {
		if length == 0 || length > 2*metadataBatchMaxBytes || int64(length) > header.IndexLength-offset {
			return fmt.Errorf("invalid v2 index frame length")
		}
		offsets[i] = offset
		offset += int64(length)
	}
	if offset != header.IndexLength {
		return fmt.Errorf("v2 index frame boundary mismatch")
	}
	batches := make([][]common.ClipNode, count)
	group := new(errgroup.Group)
	group.SetLimit(min(16, runtime.GOMAXPROCS(0)))
	for i, length := range lengths {
		section := io.NewSectionReader(file, header.IndexPos+offsets[i], int64(length))
		group.Go(func() error {
			compressed := make([]byte, length)
			if _, err := io.ReadFull(section, compressed); err != nil {
				return err
			}
			decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(metadataBatchMaxBytes))
			if err != nil {
				return err
			}
			defer decoder.Close()
			data, err := decoder.DecodeAll(compressed, nil)
			if err != nil {
				return err
			}
			batches[i], err = decodeNodeBatch(data)
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	for _, batch := range batches {
		for i := range batch {
			index.Load(&batch[i])
		}
	}
	return nil
}

func encodeNodeBatch(nodes []*common.ClipNode) []byte {
	data := binary.AppendUvarint(nil, uint64(len(nodes)))
	for _, n := range nodes {
		for _, s := range []string{string(n.NodeType), n.Path, n.Target, n.ContentHash} {
			data = binary.AppendUvarint(data, uint64(len(s)))
			data = append(data, s...)
		}
		for _, value := range []uint64{n.Attr.Ino, n.Attr.Size, n.Attr.Blocks, n.Attr.Atime, n.Attr.Mtime, n.Attr.Ctime, uint64(n.Attr.Atimensec), uint64(n.Attr.Mtimensec), uint64(n.Attr.Ctimensec), uint64(n.Attr.Mode), uint64(n.Attr.Nlink), uint64(n.Attr.Uid), uint64(n.Attr.Gid), uint64(n.Attr.Rdev), uint64(n.Attr.Blksize), uint64(n.Attr.Padding)} {
			data = binary.AppendUvarint(data, value)
		}
		data = binary.AppendVarint(data, n.DataPos)
		data = binary.AppendVarint(data, n.DataLen)
		if n.Remote == nil {
			data = append(data, 0)
			continue
		}
		data = append(data, 1)
		data = binary.AppendUvarint(data, uint64(len(n.Remote.LayerDigest)))
		data = append(data, n.Remote.LayerDigest...)
		data = binary.AppendVarint(data, n.Remote.UOffset)
		data = binary.AppendVarint(data, n.Remote.ULength)
	}
	return data
}

type nodeBatchReader struct {
	data []byte
	err  error
}

func (r *nodeBatchReader) uint() uint64 {
	if r.err != nil {
		return 0
	}
	value, n := binary.Uvarint(r.data)
	if n <= 0 {
		r.err = fmt.Errorf("invalid metadata integer")
		return 0
	}
	r.data = r.data[n:]
	return value
}
func (r *nodeBatchReader) int() int64 { value := r.uint(); return int64(value>>1) ^ -int64(value&1) }
func (r *nodeBatchReader) bytes() []byte {
	length := r.uint()
	if length > uint64(len(r.data)) {
		r.err = io.ErrUnexpectedEOF
		return nil
	}
	value := r.data[:length]
	r.data = r.data[length:]
	return value
}
func (r *nodeBatchReader) string() string { return string(r.bytes()) }

func internMetadataString(data []byte, known map[string]string) string {
	value := known[string(data)]
	if value == "" {
		value = string(data)
		known[value] = value
	}
	return value
}

func decodeNodeBatch(data []byte) ([]common.ClipNode, error) {
	r := nodeBatchReader{data: data}
	count := r.uint()
	if count > metadataBatchSize || count > uint64(len(r.data))/23 {
		return nil, fmt.Errorf("invalid metadata node count")
	}
	nodes := make([]common.ClipNode, count)
	remotes := make([]common.RemoteRef, count)
	known := make(map[string]string)
	for i := range nodes {
		n := &nodes[i]
		n.NodeType = common.ClipNodeType(internMetadataString(r.bytes(), known))
		n.Path = r.string()
		n.Target = r.string()
		n.ContentHash = r.string()
		var a [16]uint64
		for j := range a {
			a[j] = r.uint()
			if j >= 6 && a[j] > uint64(^uint32(0)) {
				return nil, fmt.Errorf("metadata attribute overflow")
			}
		}
		n.Attr = fuse.Attr{Ino: a[0], Size: a[1], Blocks: a[2], Atime: a[3], Mtime: a[4], Ctime: a[5], Atimensec: uint32(a[6]), Mtimensec: uint32(a[7]), Ctimensec: uint32(a[8]), Mode: uint32(a[9]), Nlink: uint32(a[10]), Owner: fuse.Owner{Uid: uint32(a[11]), Gid: uint32(a[12])}, Rdev: uint32(a[13]), Blksize: uint32(a[14]), Padding: uint32(a[15])}
		n.DataPos = r.int()
		n.DataLen = r.int()
		switch r.uint() {
		case 0:
		case 1:
			remotes[i] = common.RemoteRef{LayerDigest: internMetadataString(r.bytes(), known), UOffset: r.int(), ULength: r.int()}
			n.Remote = &remotes[i]
		default:
			return nil, fmt.Errorf("invalid metadata remote flag")
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.data) != 0 {
		return nil, fmt.Errorf("trailing metadata node data")
	}
	return nodes, nil
}

// TranscodeMetadata derives a deterministic batched OCI metadata archive without
// changing its source. A nil metadata argument loads the source index first.
func (ca *ClipArchiver) TranscodeMetadata(sourcePath, destinationPath string, metadata *common.ClipArchiveMetadata) error {
	var err error
	if metadata == nil {
		metadata, err = ca.ExtractMetadata(sourcePath)
		if err != nil {
			return err
		}
	}
	if metadata.Header.ClipFileFormatVersion != common.ClipFileFormatVersion || metadata.StorageInfo == nil || metadata.StorageInfo.Type() != string(common.StorageModeOCI) {
		return fmt.Errorf("batched metadata requires a legacy OCI archive")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil {
		return err
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	destinationAbs, err := filepath.Abs(destinationPath)
	if err != nil {
		return err
	}
	if sourceAbs == destinationAbs {
		return fmt.Errorf("source and destination archives must differ")
	}
	if destinationStat, err := os.Stat(destinationPath); err == nil && os.SameFile(stat, destinationStat) {
		return fmt.Errorf("source and destination archives must differ")
	}
	headerBytes := make([]byte, common.ClipHeaderLength)
	if _, err := source.ReadAt(headerBytes, 0); err != nil {
		return err
	}
	sourceHeader, err := ca.DecodeHeader(headerBytes)
	if err != nil {
		return err
	}
	if *sourceHeader != metadata.Header {
		return fmt.Errorf("metadata header does not match source archive")
	}
	header := metadata.Header
	if header.StorageInfoPos < 0 || header.StorageInfoLength < 0 || header.StorageInfoPos > stat.Size() || header.StorageInfoLength > stat.Size()-header.StorageInfoPos {
		return fmt.Errorf("invalid source storage info boundary")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return err
	}
	index, err := ca.encodeIndexV2(metadata.Index, hash.Sum(nil), stat.Size())
	if err != nil {
		return err
	}
	header.ClipFileFormatVersion = batchedMetadataVersion
	header.IndexPos = common.ClipHeaderLength
	header.IndexLength = int64(len(index))
	header.StorageInfoPos = header.IndexPos + header.IndexLength
	encoded, err := ca.EncodeHeader(&header)
	if err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(destinationPath), ".clip-batch-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	if _, err := output.Write(encoded); err != nil {
		return err
	}
	if _, err := output.Write(index); err != nil {
		return err
	}
	if _, err := io.CopyN(output, io.NewSectionReader(source, metadata.Header.StorageInfoPos, metadata.Header.StorageInfoLength), metadata.Header.StorageInfoLength); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(output.Name(), destinationPath)
}
