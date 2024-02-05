package archive

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
	"strings"
	"syscall"

	log "github.com/okteto/okteto/pkg/log"

	common "github.com/beam-cloud/clip/pkg/common"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/karrick/godirwalk"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&common.ClipNode{})
	gob.Register(&common.StorageInfoWrapper{})
	gob.Register(&common.S3StorageInfo{})

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

// populateIndex creates an representation of the filesystem/folder being archived
func (ca *ClipArchiver) populateIndex(index *btree.BTree, sourcePath string) error {
	root := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Mode: uint32(os.ModeDir | 0755),
		},
	}
	index.Set(root)

	err := godirwalk.Walk(sourcePath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
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

			var err error
			var fi fs.FileInfo
			if nodeType == common.SymLinkNode {
				fi, err = os.Lstat(path)
				if err != nil {
					return err
				}
			} else {
				fi, err = os.Stat(path)
				if err != nil {
					return err
				}
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

			mode := uint32(fi.Mode())
			if fi.IsDir() {
				mode |= syscall.S_IFDIR
			} else if de.IsSymlink() {
				mode |= syscall.S_IFLNK
			} else {
				mode |= syscall.S_IFREG
			}

			attr := fuse.Attr{
				Ino:    fi.Sys().(*syscall.Stat_t).Ino,
				Size:   uint64(fi.Size()),
				Blocks: uint64(fi.Sys().(*syscall.Stat_t).Blocks),
				Atime:  uint64(fi.ModTime().Unix()),
				Mtime:  uint64(fi.ModTime().Unix()),
				Mode:   mode,
				Nlink:  uint32(fi.Sys().(*syscall.Stat_t).Nlink),
				Owner: fuse.Owner{
					Uid: fi.Sys().(*syscall.Stat_t).Uid,
					Gid: fi.Sys().(*syscall.Stat_t).Gid,
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
		case "s3":
			var s3Info common.S3StorageInfo
			if err := gob.NewDecoder(bytes.NewReader(wrapper.Data)).Decode(&s3Info); err != nil {
				return nil, fmt.Errorf("error decoding s3 storage info: %v", err)
			}
			storageInfo = s3Info
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
			log.Spinner(fmt.Sprintf("Extracting... %s", node.Path))
		}

		if node.NodeType == common.FileNode {
			// Seek to the position of the file in the archive
			_, err := file.Seek(node.DataPos, 0)
			if err != nil {
				log.Printf("error seeking to file %s: %v", node.Path, err)
				return false
			}

			// Open the output file
			outFile, err := os.Create(path.Join(opts.OutputPath, node.Path))
			if err != nil {
				if opts.Verbose {
					log.Printf("error creating file %s: %v", node.Path, err)
				}
				return false
			}
			defer outFile.Close()

			// Copy the data from the archive to the output file
			_, err = io.CopyN(outFile, file, node.DataLen)
			if err != nil {
				if opts.Verbose {
					log.Printf("error extracting file %s: %v", node.Path, err)
				}
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
		log.Spinner(fmt.Sprintf("Archiving... %s", node.Path))
	}

	f, err := os.Open(path.Join(sourcePath, node.Path))
	if err != nil {
		log.Printf("error opening source file %s: %v", node.Path, err)
		return false
	}
	defer f.Close()

	// Initialize CRC64 table and hash
	table := crc64.MakeTable(crc64.ISO)
	hash := crc64.New(table)

	blockType := common.BlockTypeFile

	// Write block type
	if err := binary.Write(writer, binary.LittleEndian, blockType); err != nil {
		log.Printf("error writing block type: %v", err)
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
		log.Printf("error copying file %s: %v", node.Path, err)
		return false
	}

	// Compute final CRC64 checksum
	checksum := hash.Sum(nil)

	// Write checksum to output file
	if _, err := writer.Write(checksum); err != nil {
		log.Printf("error writing checksum: %v", err)
		return false
	}

	// Increment position to account for checksum
	*pos += ChecksumLength

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
