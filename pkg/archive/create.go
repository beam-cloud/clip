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
	cf *ClipFile
}

func NewClipArchiver() (*ClipArchiver, error) {
	return &ClipArchiver{
		cf: nil,
	}, nil
}

func (ca *ClipArchiver) CreateFromDirectory(targetPath string) (*ClipFile, error) {
	cf := NewClipFile()
	ca.cf = cf

	err := ca.populateIndex(targetPath)
	if err != nil {
		return nil, err
	}

	return cf, nil
}

func (ca *ClipArchiver) populateIndex(targetPath string) error {
	err := godirwalk.Walk(targetPath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			var target string = ""
			var nodeType NodeType

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

			ca.cf.Index.Set(&ClipFSNode{Path: strings.TrimPrefix(path, targetPath), NodeType: nodeType, Attr: attr, Target: target})

			return nil
		},
		Unsorted: false,
	})

	return err
}
