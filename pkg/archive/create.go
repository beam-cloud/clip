package archive

import (
	"io/fs"
	"os"
	"strings"
	"syscall"

	"bazil.org/fuse"
	"github.com/karrick/godirwalk"
)

type ClipArchiver struct {
	archive *ClipArchive
}

func NewClipArchiver() (*ClipArchiver, error) {
	return &ClipArchiver{
		archive: nil,
	}, nil
}

func (a *ClipArchiver) Create(sourcePath string, outputPath string) (*ClipArchive, error) {
	a.archive = NewClipArchive()

	err := a.populateIndex(sourcePath)
	if err != nil {
		return nil, err
	}

	err = a.writeIndex()
	if err != nil {
		return nil, err
	}

	err = a.writeFileBlocks()
	if err != nil {
		return nil, err
	}

	return a.archive, nil
}

// populateIndex creates an index representing the filesystem/folder structure being archived.
func (a *ClipArchiver) populateIndex(sourcePath string) error {
	err := godirwalk.Walk(sourcePath, &godirwalk.Options{
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

			a.archive.Index.Set(&ClipNode{Path: strings.TrimPrefix(path, sourcePath), NodeType: nodeType, Attr: attr, Target: target})

			return nil
		},
		Unsorted: false,
	})

	return err
}

func (a *ClipArchiver) writeHeader() error {
	return nil
}

func (a *ClipArchiver) writeIndex() error {
	return nil
}

func (a *ClipArchiver) writeFileBlocks() error {
	return nil
}
