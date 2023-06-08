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
	"log"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/karrick/godirwalk"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&ClipNode{})
	gob.Register(&ClipArchiveHeader{})
}

type ClipArchiveOptions struct {
	Compress    bool
	ArchivePath string
	SourcePath  string
	OutputFile  string
	OutputPath  string
}

type ClipArchive struct {
	Index *btree.BTree
}

func NewClipArchive() *ClipArchive {
	compare := func(a, b interface{}) bool {
		return a.(*ClipNode).Path < b.(*ClipNode).Path
	}
	return &ClipArchive{
		Index: btree.New(compare),
	}
}

// createIndex creates an representation of the filesystem/folder structure being archived
func (ca *ClipArchive) createIndex(sourcePath string) error {
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
				// Nlink:  uint32(fi.Sys().(*syscall.Stat_t).Nlink),
				// Uid:    uint32(fi.Sys().(*syscall.Stat_t).Uid),
				// Gid:    uint32(fi.Sys().(*syscall.Stat_t).Gid),
			}

			ca.Index.Set(&ClipNode{Path: strings.TrimPrefix(path, sourcePath), NodeType: nodeType, Attr: attr, Target: target})

			return nil
		},
		Unsorted: false,
	})

	return err
}

func (ca *ClipArchive) Insert(node *ClipNode) {
	ca.Index.Set(node)
}

func (ca *ClipArchive) Get(path string) *ClipNode {
	item := ca.Index.Get(&ClipNode{Path: path})
	if item == nil {
		return nil
	}
	return item.(*ClipNode)
}

func (ca *ClipArchive) ListDirectory(path string) []*ClipNode {
	var entries []*ClipNode

	// Append null character to the path -- if we don't do this we could miss some child nodes.
	// It works because \x00 is lower lexagraphically than any other character
	pivot := &ClipNode{Path: path + "\x00"}
	ca.Index.Ascend(pivot, func(a interface{}) bool {
		node := a.(*ClipNode)

		// Remove the prefix and check if there are any "/" left
		relativePath := strings.TrimPrefix(node.Path, path)
		if strings.Contains(relativePath, "/") {
			// This node is not an immediate child, continue on
			return true
		}

		// Node is an immediate child, so we append it to entries
		if relativePath != "" {
			entries = append(entries, node)
		}

		return true
	})

	return entries
}

func (ca *ClipArchive) Create(opts ClipArchiveOptions) error {
	outFile, err := os.Create(opts.OutputFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	err = ca.createIndex(opts.SourcePath)
	if err != nil {
		return err
	}

	// Prepare and write placeholder for the header
	header := ClipArchiveHeader{
		StartBytes:            ClipFileStartBytes,
		ClipFileFormatVersion: ClipFileFormatVersion,
		IndexSize:             0,
	}

	headerPos, err := outFile.Seek(0, os.SEEK_CUR) // Get current position
	if err != nil {
		return err
	}

	// Write placeholder bytes for the header
	if _, err := outFile.Write(make([]byte, ClipHeaderLength)); err != nil {
		return err
	}

	// Write data blocks
	var initialOffset int64 = int64(ClipHeaderLength)
	err = ca.writeBlocks(opts.SourcePath, outFile, initialOffset)
	if err != nil {
		return err
	}

	// Write the actual index data
	indexPos, err := outFile.Seek(0, os.SEEK_CUR) // Get current position
	if err != nil {
		return err
	}

	indexBytes, err := ca.encodeIndex()
	if err != nil {
		return err
	}

	if _, err := outFile.Write(indexBytes); err != nil {
		return err
	}

	// Update the header with the correct index size and position
	header.IndexSize = int64(len(indexBytes))
	header.IndexPos = indexPos

	headerBytes, err := ca.encodeHeader(&header)
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

func (ca *ClipArchive) Extract(opts ClipArchiveOptions) error {
	file, err := os.Open(opts.ArchivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	os.MkdirAll(opts.OutputPath, 0755)

	// Read and decode the header
	headerBytes := make([]byte, ClipHeaderLength)
	if _, err := io.ReadFull(file, headerBytes); err != nil {
		return fmt.Errorf("error reading header: %v", err)
	}

	// Decode the header
	headerReader := bytes.NewReader(headerBytes)
	headerDec := gob.NewDecoder(headerReader)
	var header ClipArchiveHeader
	if err := headerDec.Decode(&header); err != nil {
		return fmt.Errorf("error decoding header: %v", err)
	}

	// Verify the header
	if !bytes.Equal(header.StartBytes, ClipFileStartBytes) || header.ClipFileFormatVersion != ClipFileFormatVersion {
		return common.ErrFileHeaderMismatch
	}

	// Seek to the correct position for the index
	_, err = file.Seek(header.IndexPos, 0)
	if err != nil {
		return fmt.Errorf("error seeking to index: %v", err)
	}

	// Read and decode the index
	indexBytes := make([]byte, header.IndexSize)
	if _, err := io.ReadFull(file, indexBytes); err != nil {
		return fmt.Errorf("error reading index: %v", err)
	}

	indexReader := bytes.NewReader(indexBytes)
	indexDec := gob.NewDecoder(indexReader)

	var nodes []*ClipNode
	if err := indexDec.Decode(&nodes); err != nil {
		return fmt.Errorf("error decoding index: %v", err)
	}

	for _, node := range nodes {
		ca.Index.Set(node)
	}

	// Iterate over the index and extract every node
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)

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
				log.Printf("error creating file %s: %v", node.Path, err)
				return false
			}
			defer outFile.Close()

			// Copy the data from the archive to the output file
			_, err = io.CopyN(outFile, file, node.DataLen)
			if err != nil {
				log.Printf("error extracting file %s: %v", node.Path, err)
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

func (ca *ClipArchive) writeBlocks(sourcePath string, outFile *os.File, offset int64) error {
	writer := bufio.NewWriterSize(outFile, 512*1024)
	defer writer.Flush() // Ensure all data gets written when we're done

	var pos int64 = offset
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)

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

func (ca *ClipArchive) encodeHeader(header *ClipArchiveHeader) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(header); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (ca *ClipArchive) encodeIndex() ([]byte, error) {
	var nodes []*ClipNode
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
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
