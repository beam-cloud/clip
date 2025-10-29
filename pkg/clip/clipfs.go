package clip

import (
	"fmt"
	"sync"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog/log"
)

type ClipFileSystemOpts struct {
	ContentCache          ContentCache
	ContentCacheAvailable bool
}

type ClipFileSystem struct {
	storage               storage.ClipStorageInterface
	root                  *FSNode
	lookupCache           map[string]*lookupCacheEntry
	contentCache          ContentCache
	contentCacheAvailable bool
	cacheMutex            sync.RWMutex
	cachingStatus         map[string]bool
	cacheEventChan        chan cacheEvent
	cachingStatusMu       sync.Mutex
}

type lookupCacheEntry struct {
	inode *fs.Inode
	attr  fuse.Attr
}

type ContentCache interface {
	GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error)
	StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error)
}

type cacheEvent struct {
	node *FSNode
}

func NewFileSystem(s storage.ClipStorageInterface, opts ClipFileSystemOpts) (*ClipFileSystem, error) {
	cfs := &ClipFileSystem{
		storage:               s,
		lookupCache:           make(map[string]*lookupCacheEntry),
		contentCache:          opts.ContentCache,
		cacheEventChan:        make(chan cacheEvent, 10000),
		cachingStatus:         make(map[string]bool),
		contentCacheAvailable: opts.ContentCacheAvailable,
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

	go cfs.processCacheEvents()

	return cfs, nil
}

func (cfs *ClipFileSystem) Root() (fs.InodeEmbedder, error) {
	if cfs.root == nil {
		return nil, fmt.Errorf("root not initialized")
	}
	return cfs.root, nil
}

func (cfs *ClipFileSystem) CacheFile(node *FSNode) {
	hash := node.clipNode.ContentHash

	// Check and update caching status
	cfs.cachingStatusMu.Lock()
	if cfs.cachingStatus[hash] {
		cfs.cachingStatusMu.Unlock()
		return // File is already being cached or has been cached
	}
	cfs.cachingStatus[hash] = true
	cfs.cachingStatusMu.Unlock()

	// Submit cache event
	cfs.cacheEventChan <- cacheEvent{node: node}
}

func (cfs *ClipFileSystem) clearCachingStatus(hash string) {
	cfs.cachingStatusMu.Lock()
	delete(cfs.cachingStatus, hash)
	cfs.cachingStatusMu.Unlock()
}

func (cfs *ClipFileSystem) processCacheEvents() {
	for cacheEvent := range cfs.cacheEventChan {
		clipNode := cacheEvent.node.clipNode

		if clipNode.DataLen > 0 {
			chunks := make(chan []byte, 1)

			go func(chunks chan []byte) {
				chunkSize := int64(1 << 25) // 32Mb

				if chunkSize > clipNode.DataLen {
					chunkSize = clipNode.DataLen
				}

				for offset := int64(0); offset < clipNode.DataLen; offset += int64(chunkSize) {
					if (clipNode.DataLen - offset) < chunkSize {
						chunkSize = clipNode.DataLen - offset
					}

				fileContent := make([]byte, chunkSize) // Create a new buffer for each chunk
				nRead, err := cfs.storage.ReadFile(clipNode, fileContent, offset)
				if err != nil {
					log.Error().Err(err).Str("path", clipNode.Path).Msg("error reading file for caching")
					break
				}

					chunks <- fileContent[:nRead]
					fileContent = nil
				}

				close(chunks)
			}(chunks)

			hash, err := cfs.contentCache.StoreContent(chunks, clipNode.ContentHash, struct{ RoutingKey string }{RoutingKey: clipNode.ContentHash})
			if err != nil || hash != clipNode.ContentHash {
				log.Error().Err(err).Str("path", clipNode.Path).Str("hash", clipNode.ContentHash).Msg("error storing file contents")
				cfs.clearCachingStatus(clipNode.ContentHash)
			}
		}
	}
}
