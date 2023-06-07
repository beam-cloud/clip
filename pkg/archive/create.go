package archive

import (
	"strings"

	"github.com/karrick/godirwalk"
)

type ClipArchiver struct {
}

func NewClipArchiver() (*ClipArchiver, error) {
	return &ClipArchiver{}, nil
}

func (ca *ClipArchiver) CreateFromDirectory(targetPath string) (*ClipFile, error) {
	cf := NewClipFile()

	err := godirwalk.Walk(targetPath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			var nodeType NodeType
			if de.IsDir() {
				nodeType = DirNode
			} else if de.IsSymlink() {
				nodeType = SymLinkNode
			} else {
				nodeType = FileNode
			}

			cf.Index.Set(&ClipFSNode{Path: strings.TrimPrefix(path, targetPath), NodeType: nodeType})

			return nil
		},
		Unsorted: true, // (optional) set true for faster yet non-deterministic enumeration (see godoc)
	})

	if err != nil {
		return nil, err
	}

	return cf, nil
}
