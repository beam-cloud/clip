package archive

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"hash/crc64"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	log "github.com/okteto/okteto/pkg/log"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/karrick/godirwalk"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&ClipNode{})
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
		return a.(*ClipNode).Path < b.(*ClipNode).Path
	}
	return btree.New(compare)
}

// populateIndex creates an representation of the filesystem/folder structure being archived
func (ca *ClipArchiver) populateIndex(index *btree.BTree, sourcePath string) error {
	err := godirwalk.Walk(sourcePath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			var target string = ""
			var nodeType ClipNodeType

			if de.IsDir() {
				nodeType = DirNode
			} else if de.IsSymlink() {
				_target, err := os.Readlink(path)
				if err != nil {
					return fmt.Errorf("error reading symlink target %s: %v", path, err)
				}

				target = _target
				nodeType = SymLinkNode
			} else {
				nodeType = FileNode
			}

			var err error
			var fi fs.FileInfo
			if nodeType == SymLinkNode {
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
			attr := fuse.Attr{
				Ino:    fi.Sys().(*syscall.Stat_t).Ino,
				Size:   uint64(fi.Size()),
				Blocks: uint64(fi.Sys().(*syscall.Stat_t).Blocks),
				Atime:  uint64(fi.ModTime().Unix()),
				Mtime:  uint64(fi.ModTime().Unix()),
				Mode:   uint32(fi.Mode().Perm()),
				Nlink:  uint32(fi.Sys().(*syscall.Stat_t).Nlink),
				Owner: fuse.Owner{
					Uid: fi.Sys().(*syscall.Stat_t).Uid,
					Gid: fi.Sys().(*syscall.Stat_t).Gid,
				},
			}

			index.Set(&ClipNode{Path: strings.TrimPrefix(path, sourcePath), NodeType: nodeType, Attr: attr, Target: target})

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
	header := ClipArchiveHeader{
		ClipFileFormatVersion: ClipFileFormatVersion,
		IndexLength:           0,
		StorageInfoLength:     0,
		StorageInfoPos:        0,
	}
	copy(header.StartBytes[:], ClipFileStartBytes)

	headerPos, err := outFile.Seek(0, io.SeekCurrent) // Get current position
	if err != nil {
		return err
	}

	// Write placeholder bytes for the header
	if _, err := outFile.Write(make([]byte, ClipHeaderLength)); err != nil {
		return err
	}

	// Write data blocks
	var initialOffset int64 = int64(ClipHeaderLength)
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

func (ca *ClipArchiver) ExtractMetadata(archivePath string) (*ClipArchiveMetadata, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Read and decode the header
	headerBytes := make([]byte, ClipHeaderLength)
	if _, err := io.ReadFull(file, headerBytes); err != nil {
		return nil, common.ErrFileHeaderMismatch
	}

	// Decode the header
	header, err := ca.DecodeHeader(headerBytes)
	if err != nil {
		return nil, common.ErrFileHeaderMismatch
	}

	// Verify the header
	if !bytes.Equal(header.StartBytes[:], ClipFileStartBytes) || header.ClipFileFormatVersion != ClipFileFormatVersion {
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

	var nodes []*ClipNode
	if err := indexDec.Decode(&nodes); err != nil {
		return nil, fmt.Errorf("error decoding index: %v", err)
	}

	index := ca.newIndex()
	for _, node := range nodes {
		index.Set(node)
	}

	return &ClipArchiveMetadata{
		Index:  index,
		Header: *header,
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
	headerBytes := make([]byte, ClipHeaderLength)
	if _, err := io.ReadFull(file, headerBytes); err != nil {
		return common.ErrFileHeaderMismatch
	}

	// Decode the header
	header, err := ca.DecodeHeader(headerBytes)
	if err != nil {
		return common.ErrFileHeaderMismatch
	}

	// Verify the header
	if !bytes.Equal(header.StartBytes[:], ClipFileStartBytes) || header.ClipFileFormatVersion != ClipFileFormatVersion {
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

	var nodes []*ClipNode
	if err := indexDec.Decode(&nodes); err != nil {
		return fmt.Errorf("error decoding index: %v", err)
	}

	index := ca.newIndex()
	for _, node := range nodes {
		index.Set(node)
	}

	// Iterate over the index and extract every node
	index.Ascend(index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)

		if opts.Verbose {
			log.Spinner(fmt.Sprintf("Extracting... %s", node.Path))
		}

		if node.NodeType == FileNode {
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

		} else if node.NodeType == DirNode {
			os.MkdirAll(path.Join(opts.OutputPath, node.Path), fs.FileMode(node.Attr.Mode))
		} else if node.NodeType == SymLinkNode {
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
	index.Ascend(index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)

		if opts.Verbose {
			log.Spinner(fmt.Sprintf("Archiving... %s", node.Path))
		}

		if node.NodeType == FileNode {
			f, err := os.Open(path.Join(sourcePath, node.Path))
			if err != nil {
				log.Printf("error opening source file %s: %v", node.Path, err)
				return false
			}
			defer f.Close()

			// Initialize CRC64 table and hash
			table := crc64.MakeTable(crc64.ISO)
			hash := crc64.New(table)

			blockType := blockTypeFile

			// Write block type
			if err := binary.Write(writer, binary.LittleEndian, blockType); err != nil {
				log.Printf("error writing block type: %v", err)
				return false
			}

			// Increment position to account for block type
			pos += 1

			// Update data position
			node.DataPos = pos

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
			pos += ChecksumLength

			// Update node with data length
			node.DataLen = copied

			pos += copied
		}

		return true
	})

	return nil
}

func (ca *ClipArchiver) EncodeHeader(header *ClipArchiveHeader) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (ca *ClipArchiver) DecodeHeader(headerBytes []byte) (*ClipArchiveHeader, error) {
	header := new(ClipArchiveHeader)
	buf := bytes.NewBuffer(headerBytes)
	if err := binary.Read(buf, binary.LittleEndian, header); err != nil {
		return nil, err
	}
	return header, nil
}

func (ca *ClipArchiver) EncodeIndex(index *btree.BTree) ([]byte, error) {
	var nodes []*ClipNode
	index.Ascend(index.Min(), func(a interface{}) bool {
		nodes = append(nodes, a.(*ClipNode))
		return true
	})

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(nodes); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
