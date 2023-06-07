package archive

import (
	"strings"

	"github.com/karrick/godirwalk"
)

func PopulateFromDirectory(fs *FileSystem, targetPath string) error {
	return godirwalk.Walk(targetPath, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			var nodeType NodeType
			if de.IsDir() {
				nodeType = DirNode
			} else if de.IsSymlink() {
				nodeType = SymLinkNode
			} else {
				nodeType = FileNode
			}

			fs.tree.Set(&FsNode{Path: strings.TrimPrefix(path, targetPath), NodeType: nodeType})

			return nil
		},
		Unsorted: true, // (optional) set true for faster yet non-deterministic enumeration (see godoc)
	})
}
