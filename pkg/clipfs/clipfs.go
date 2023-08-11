package clipfs

import (
	"fmt"
	"log"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type ProfilingData struct {
	Calls map[string]int
	Times map[string]time.Duration
}

type ClipFileSystem struct {
	s             storage.ClipStorageInterface
	root          *FSNode
	verbose       bool
	profilingData *ProfilingData
}

func (cfs *ClipFileSystem) PrintProfilingData() {
	for name, calls := range cfs.profilingData.Calls {
		totalTime := cfs.profilingData.Times[name]
		avgTime := totalTime / time.Duration(calls)
		log.Printf("Profile %s: %d calls, total time: %v, average time per call: %v\n", name, calls, totalTime, avgTime)
	}
}

func NewFileSystem(s storage.ClipStorageInterface, verbose bool) (*ClipFileSystem, error) {
	cfs := &ClipFileSystem{
		s:       s,
		verbose: verbose,
		profilingData: &ProfilingData{
			Calls: make(map[string]int),
			Times: make(map[string]time.Duration),
		},
	}

	metadata := s.Metadata()
	rootNode := metadata.Get("/")
	if rootNode == nil {
		return nil, common.ErrMissingArchiveRoot
	}

	cfs.root = &FSNode{
		filesystem: cfs,
		attr:       rootNode.Attr,
		clipNode:   rootNode,
	}

	if verbose {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()

			for {
				<-ticker.C
				cfs.PrintProfilingData()
			}
		}()
	}

	return cfs, nil
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}
