package archive

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"strings"
	"syscall"

	"bazil.org/fuse"
	"github.com/karrick/godirwalk"
	"github.com/tidwall/btree"
)

func init() {
	gob.Register(&ClipNode{})
	gob.Register(&ClipArchiveHeader{})
}

type ClipArchive struct {
	SourcePath string
	Index      *btree.BTree
}

func NewClipArchive(sourcePath string) *ClipArchive {
	compare := func(a, b interface{}) bool {
		return a.(*ClipNode).Path < b.(*ClipNode).Path
	}
	return &ClipArchive{
		SourcePath: sourcePath,
		Index:      btree.New(compare),
	}
}

// CreateIndex creates an representation of the filesystem/folder structure being archived
func (ca *ClipArchive) CreateIndex() error {
	err := godirwalk.Walk(ca.SourcePath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			var target string = ""
			var nodeType ClipNodeType

			if de.IsDir() {
				nodeType = DirNode
			} else if de.IsSymlink() {
				target = target
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
				Inode:     uint64(fi.Sys().(*syscall.Stat_t).Ino),
				Size:      uint64(fi.Size()),
				Blocks:    uint64(fi.Sys().(*syscall.Stat_t).Blocks),
				Atime:     fi.ModTime(),
				Mtime:     fi.ModTime(),
				Mode:      fi.Mode(),
				Nlink:     uint32(fi.Sys().(*syscall.Stat_t).Nlink),
				Uid:       uint32(fi.Sys().(*syscall.Stat_t).Uid),
				Gid:       uint32(fi.Sys().(*syscall.Stat_t).Gid),
				BlockSize: uint32(fi.Sys().(*syscall.Stat_t).Blksize),
				// Flags:     fuse.AttrFlags{}, // Assuming no specific flags at this point
			}

			ca.Index.Set(&ClipNode{Path: strings.TrimPrefix(path, ca.SourcePath), NodeType: nodeType, Attr: attr, Target: target})

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

func (ca *ClipArchive) Load(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	var nodes []*ClipNode
	if err := dec.Decode(&nodes); err != nil {
		return err
	}

	for _, node := range nodes {
		ca.Index.Set(node)
	}

	return nil
}

func (ca *ClipArchive) PrintNodes() {
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)
		fmt.Printf("Path: %s, NodeType: %s, count: %d\n", node.Path, node.NodeType)
		return true
	})
}

func (ca *ClipArchive) Dump(targetFile string) error {
	outFile, err := os.Create(targetFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// Prepare and write header
	header := ClipArchiveHeader{
		StartBytes:            ClipFileStartBytes,
		ClipFileFormatVersion: ClipFileFormatVersion,
		IndexSize:             0,
	}

	indexBytes, err := ca.encodeIndex()
	if err != nil {
		return err
	}

	header.IndexSize = len(indexBytes)
	headerBytes, err := ca.encodeHeader(&header)
	if err != nil {
		return err
	}

	if _, err := outFile.Write(headerBytes); err != nil {
		return err
	}

	// Write a placeholder for the index
	indexPos, err := outFile.Seek(0, os.SEEK_CUR) // Get current position
	if err != nil {
		return err
	}

	// Skip the index space
	if _, err := outFile.Seek(int64(header.IndexSize), os.SEEK_CUR); err != nil {
		return err
	}

	// Write data
	err = ca.writeBlocks(outFile)
	if err != nil {
		return err
	}

	// Write the actual index data
	_, err = outFile.Seek(indexPos, os.SEEK_SET) // Go back to index position
	if err != nil {
		return err
	}

	// TODO: don't write the index twice -- come up with a better solution here
	indexBytes, err = ca.encodeIndex()
	if err != nil {
		return err
	}

	if _, err := outFile.Write(indexBytes); err != nil {
		return err
	}

	return nil
}

func (ca *ClipArchive) writeBlocks(outFile *os.File) error {
	writer := bufio.NewWriterSize(outFile, 512*1024)
	defer writer.Flush() // Ensure all data gets written when we're done

	var pos int64 = 0
	ca.Index.Ascend(ca.Index.Min(), func(a interface{}) bool {
		node := a.(*ClipNode)

		if node.NodeType == FileNode {
			f, err := os.Open(path.Join(ca.SourcePath, node.Path))
			if err != nil {
				log.Printf("error opening file %s: %v", node.Path, err)
				return false
			}
			defer f.Close()

			_, err = io.Copy(writer, f)
			if err != nil {
				log.Printf("error copying file %s: %v", node.Path, err)
				return false
			}

			// Update each node with starting position and data length
			node.DataPos = pos
			node.DataLen = int64(node.Attr.Size)

			pos += int64(node.Attr.Size)
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
