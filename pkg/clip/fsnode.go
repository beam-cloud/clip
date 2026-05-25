package clip

import (
	"context"
	"os"
	"path"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog/log"
)

type FSNode struct {
	fs.Inode
	filesystem *ClipFileSystem
	clipNode   *common.ClipNode
	attr       fuse.Attr
}

const clipFileHandleFDCacheSize = 2048

type clipFileHandle struct {
	node  *FSNode
	mu    sync.Mutex
	files map[string]*os.File
}

func newClipFileHandle(node *FSNode) *clipFileHandle {
	return &clipFileHandle{
		node:  node,
		files: make(map[string]*os.File),
	}
}

func (fh *clipFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if caller, ok := fuse.FromContext(ctx); ok && caller != nil {
		ctx = common.WithReadTraceCallerPID(ctx, caller.Pid)
	}
	if res, ok, errno := fh.readClientLocalFileView(ctx, dest, off); ok || errno != fs.OK {
		return res, errno
	}
	return fh.node.readData(ctx, dest, off)
}

func (fh *clipFileHandle) Release(ctx context.Context) syscall.Errno {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	var firstErr syscall.Errno
	for path, file := range fh.files {
		if err := file.Close(); err != nil && firstErr == fs.OK {
			firstErr = fs.ToErrno(err)
		}
		delete(fh.files, path)
	}
	return firstErr
}

func (fh *clipFileHandle) openViewFile(path string) (*os.File, error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if file := fh.files[path]; file != nil {
		return file, nil
	}
	if len(fh.files) >= clipFileHandleFDCacheSize {
		return nil, syscall.EMFILE
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fh.files[path] = file
	return file, nil
}

func (fh *clipFileHandle) readClientLocalFileView(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, bool, syscall.Errno) {
	if fh == nil || fh.node == nil || fh.node.filesystem == nil || len(dest) == 0 {
		return nil, false, fs.OK
	}
	viewer, ok := fh.node.filesystem.storage.(storage.ClientLocalFileViewer)
	if !ok || viewer == nil {
		return nil, false, fs.OK
	}

	readLen := fh.node.clampedReadLength(off, int64(len(dest)))
	if readLen <= 0 {
		return fuse.ReadResultData(dest[:0]), true, fs.OK
	}

	started := time.Now()
	view, ok, err := viewer.ClientLocalFileView(ctx, fh.node.clipNode, off, readLen)
	if err != nil {
		fh.node.observeRead(ctx, fh.node.clientLocalFileViewReadTrace(view, off, readLen, 0, started, err))
		return nil, false, fs.OK
	}
	if !ok || view.Path == "" || view.Length <= 0 {
		return nil, false, fs.OK
	}
	if int64(view.Length) != readLen {
		return nil, false, fs.OK
	}

	file, err := fh.openViewFile(view.Path)
	if err != nil {
		fh.node.observeRead(ctx, fh.node.clientLocalFileViewReadTrace(view, off, readLen, 0, started, err))
		return nil, false, fs.OK
	}

	fh.node.observeRead(ctx, fh.node.clientLocalFileViewReadTrace(view, off, readLen, int64(view.Length), started, nil))
	return fuse.ReadResultFd(file.Fd(), view.Offset, view.Length), true, fs.OK
}

func (n *FSNode) OnAdd(ctx context.Context) {
	log.Debug().Str("path", n.clipNode.Path).Msg("OnAdd called")
}

func (n *FSNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	log.Debug().Str("path", n.clipNode.Path).Msg("Getattr called")

	node := n.clipNode

	// Fill in the AttrOut struct
	out.Ino = node.Attr.Ino
	out.Size = node.Attr.Size
	out.Blocks = node.Attr.Blocks
	out.Atime = node.Attr.Atime
	out.Mtime = node.Attr.Mtime
	out.Ctime = node.Attr.Ctime
	out.Mode = node.Attr.Mode
	out.Nlink = node.Attr.Nlink
	out.Owner = node.Attr.Owner

	return fs.OK
}

func (n *FSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Str("name", name).Msg("Lookup called")

	// Create the full path of the child node
	childPath := path.Join(n.clipNode.Path, name)

	// Check the cache
	n.filesystem.cacheMutex.RLock()
	entry, found := n.filesystem.lookupCache[childPath]
	n.filesystem.cacheMutex.RUnlock()
	if found {
		log.Debug().Str("path", childPath).Msg("Lookup cache hit")
		out.Attr = entry.attr
		return entry.inode, fs.OK
	}

	// Lookup the child node
	child := n.filesystem.storage.Metadata().Get(childPath)
	if child == nil {
		// No child with the requested name exists
		return nil, syscall.ENOENT
	}

	// Fill out the child node's attributes
	out.Attr = child.Attr

	// Create a new Inode for the child
	childInode := n.NewInode(ctx, &FSNode{filesystem: n.filesystem, clipNode: child, attr: child.Attr}, fs.StableAttr{Mode: child.Attr.Mode, Ino: child.Attr.Ino})

	// Cache the result
	n.filesystem.cacheMutex.Lock()
	n.filesystem.lookupCache[childPath] = &lookupCacheEntry{inode: childInode, attr: child.Attr}
	n.filesystem.cacheMutex.Unlock()

	return childInode, fs.OK
}

func (n *FSNode) Opendir(ctx context.Context) syscall.Errno {
	log.Debug().Str("path", n.clipNode.Path).Msg("Opendir called")
	return 0
}

func (n *FSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Uint32("flags", flags).Msg("Open called")
	if n.clipNode == nil || n.clipNode.NodeType != common.FileNode {
		return nil, 0, fs.OK
	}
	return newClipFileHandle(n), fuse.FOPEN_KEEP_CACHE, fs.OK
}

func (n *FSNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if fh, ok := f.(*clipFileHandle); ok && fh != nil {
		return fh.Read(ctx, dest, off)
	}
	return n.readData(ctx, dest, off)
}

func (n *FSNode) readData(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Int64("offset", off).Msg("Read called")
	if caller, ok := fuse.FromContext(ctx); ok && caller != nil {
		ctx = common.WithReadTraceCallerPID(ctx, caller.Pid)
	}

	readLen := n.clampedReadLength(off, int64(len(dest)))
	if readLen <= 0 {
		return fuse.ReadResultData(dest[:0]), fs.OK
	}

	var nRead int
	var err error
	readStart := time.Now()
	readSource := "unknown"

	defer func() {
		if n.clipNode.Remote != nil {
			return
		}
		n.observeLegacyRead(ctx, common.ReadTraceEvent{
			Operation: "clip.read",
			Source:    readSource,
			Path:      n.clipNode.Path,
			Offset:    off,
			Length:    readLen,
			BytesRead: int64(nRead),
			StartedAt: readStart,
			Duration:  time.Since(readStart),
			Success:   err == nil,
			Error:     errorString(err),
			Attrs:     n.legacyReadAttrs(),
		})
	}()

	// For OCI images (v2 with Remote), delegate ALL caching to the storage layer
	// The storage layer (oci.go) handles the proper 3-tier cache hierarchy:
	//   1. Disk cache (local)
	//   2. ContentCache with layer digest (remote)
	//   3. OCI registry (download + decompress)
	if n.clipNode.Remote != nil {
		// OCI mode - storage layer handles all caching
		if contextStorage, ok := n.filesystem.storage.(storage.ContextClipStorageInterface); ok {
			nRead, err = contextStorage.ReadFileContext(ctx, n.clipNode, dest[:readLen], off)
		} else {
			nRead, err = n.filesystem.storage.ReadFile(n.clipNode, dest[:readLen], off)
		}
		if err != nil {
			return nil, syscall.EIO
		}
	} else {
		// Legacy mode - use file-level ContentCache
		// Attempt to read from cache first for legacy archives
		if n.filesystem.contentCacheAvailable && n.clipNode.ContentHash != "" && !n.filesystem.storage.CachedLocally() {
			var cacheErr error
			cacheStart := time.Now()
			if readInto, ok := n.filesystem.contentCache.(storage.ContentCacheReadInto); ok {
				var n64 int64
				n64, cacheErr = readInto.ReadContentInto(n.clipNode.ContentHash, off, dest[:readLen], struct{ RoutingKey string }{RoutingKey: n.clipNode.ContentHash})
				nRead = int(n64)
				if cacheErr == nil && n64 != readLen {
					cacheErr = syscall.EIO
				}
			} else {
				var content []byte
				content, cacheErr = n.filesystem.contentCache.GetContent(n.clipNode.ContentHash, off, readLen, struct{ RoutingKey string }{RoutingKey: n.clipNode.ContentHash})
				if cacheErr == nil {
					nRead = copy(dest, content)
				}
			}
			if cacheErr == nil {
				readSource = "content_cache"
				n.observeLegacyRead(ctx, common.ReadTraceEvent{
					Operation: "clip.content_cache_read",
					Source:    "content_cache",
					Path:      n.clipNode.Path,
					Offset:    off,
					Length:    readLen,
					BytesRead: int64(nRead),
					StartedAt: cacheStart,
					Duration:  time.Since(cacheStart),
					Success:   true,
					Attrs:     n.legacyReadAttrsWith("cache_hit", "true"),
				})
				// Cache hit - use cached content
				log.Debug().Str("path", n.clipNode.Path).Int64("offset", off).Int64("length", readLen).Msg("Cache hit")
			} else {
				n.observeLegacyRead(ctx, common.ReadTraceEvent{
					Operation: "clip.content_cache_read",
					Source:    "content_cache",
					Path:      n.clipNode.Path,
					Offset:    off,
					Length:    readLen,
					StartedAt: cacheStart,
					Duration:  time.Since(cacheStart),
					Success:   false,
					Error:     errorString(cacheErr),
					Attrs:     n.legacyReadAttrsWith("cache_hit", "false"),
				})
				// Cache miss - read from storage and populate cache
				readSource = n.legacyArchiveSource()
				nRead, err = n.readLegacyArchiveObserved(ctx, dest[:readLen], off, readLen)
				if err != nil {
					return nil, syscall.EIO
				}

				// Asynchronously cache the file for future reads
				go func() {
					n.filesystem.CacheFile(n)
				}()
				log.Debug().Str("path", n.clipNode.Path).Int64("offset", off).Int64("length", readLen).Msg("Cache miss")
			}
		} else {
			// No cache available or local storage - read directly
			readSource = n.legacyArchiveSource()
			nRead, err = n.readLegacyArchiveObserved(ctx, dest[:readLen], off, readLen)
			if err != nil {
				return nil, syscall.EIO
			}
		}
	}

	return fuse.ReadResultData(dest[:nRead]), fs.OK
}

func (n *FSNode) clampedReadLength(off int64, requested int64) int64 {
	if n == nil || n.clipNode == nil || off < 0 || requested <= 0 {
		return 0
	}
	fileSize := n.fileSize()
	if off >= fileSize || fileSize == 0 {
		return 0
	}
	maxReadable := fileSize - off
	if requested > maxReadable {
		return maxReadable
	}
	return requested
}

func (n *FSNode) fileSize() int64 {
	if n == nil || n.clipNode == nil {
		return 0
	}
	if n.clipNode.Remote != nil {
		return n.clipNode.Remote.ULength
	}
	return n.clipNode.DataLen
}

func (n *FSNode) clientLocalFileViewReadTrace(view storage.ClientLocalFileView, off int64, readLen int64, bytesRead int64, started time.Time, err error) common.ReadTraceEvent {
	operation := "clip.read"
	attrs := map[string]string{}
	if n != nil && n.clipNode != nil && n.clipNode.Remote != nil {
		operation = "clip.oci_read"
		attrs["storage_mode"] = "oci"
		attrs["content_cache_available"] = strconv.FormatBool(n.filesystem != nil && n.filesystem.contentCacheAvailable)
	} else if n != nil {
		attrs = n.legacyReadAttrs()
	}
	attrs["fd_fast_path"] = "true"
	if view.Path != "" {
		attrs["client_local_file_view_path"] = view.Path
	}

	success := err == nil
	return common.ReadTraceEvent{
		Operation:        operation,
		Source:           view.Source,
		Path:             n.clipNode.Path,
		LayerDigest:      view.LayerDigest,
		DecompressedHash: view.DecompressedHash,
		Offset:           off,
		Length:           readLen,
		BytesRead:        bytesRead,
		StartedAt:        started,
		Duration:         time.Since(started),
		Success:          success,
		Error:            errorString(err),
		Attrs:            attrs,
	}
}

func (n *FSNode) observeRead(ctx context.Context, event common.ReadTraceEvent) {
	if n.filesystem == nil || n.filesystem.readTraceObserver == nil {
		return
	}
	if event.StartedAt.IsZero() {
		event.StartedAt = time.Now().Add(-event.Duration)
	}
	if event.CallerPID == 0 {
		event.CallerPID = common.ReadTraceCallerPID(ctx)
	}
	n.filesystem.readTraceObserver(event)
}

func (n *FSNode) readLegacyArchiveObserved(ctx context.Context, dest []byte, off int64, readLen int64) (int, error) {
	startedAt := time.Now()
	nRead, err := n.filesystem.storage.ReadFile(n.clipNode, dest, off)
	n.observeLegacyRead(ctx, common.ReadTraceEvent{
		Operation: "clip.archive_read",
		Source:    n.legacyArchiveSource(),
		Path:      n.clipNode.Path,
		Offset:    off,
		Length:    readLen,
		BytesRead: int64(nRead),
		StartedAt: startedAt,
		Duration:  time.Since(startedAt),
		Success:   err == nil,
		Error:     errorString(err),
		Attrs:     n.legacyReadAttrs(),
	})
	return nRead, err
}

func (n *FSNode) observeLegacyRead(ctx context.Context, event common.ReadTraceEvent) {
	n.observeRead(ctx, event)
}

func (n *FSNode) legacyReadAttrs() map[string]string {
	attrs := map[string]string{
		"storage_mode": "legacy",
	}
	if n.filesystem != nil {
		attrs["content_cache_available"] = strconv.FormatBool(n.filesystem.contentCacheAvailable)
	}
	if n.clipNode.ContentHash != "" {
		attrs["content_hash"] = n.clipNode.ContentHash
		if len(n.clipNode.ContentHash) > 12 {
			attrs["content_hash_short"] = n.clipNode.ContentHash[:12]
		}
	}
	if n.filesystem != nil && n.filesystem.storage != nil {
		attrs["cached_locally"] = strconv.FormatBool(n.filesystem.storage.CachedLocally())
	}
	return attrs
}

func (n *FSNode) legacyReadAttrsWith(key, value string) map[string]string {
	attrs := n.legacyReadAttrs()
	if key != "" {
		attrs[key] = value
	}
	return attrs
}

func (n *FSNode) legacyArchiveSource() string {
	if n.filesystem != nil && n.filesystem.storage != nil && n.filesystem.storage.CachedLocally() {
		return "local_archive"
	}
	return "remote_archive"
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (n *FSNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Msg("Readlink called")

	if n.clipNode.NodeType != common.SymLinkNode {
		// This node is not a symlink
		return nil, syscall.EINVAL
	}

	// Use the symlink target path directly
	symlinkTarget := n.clipNode.Target

	// In this case, we don't need to read the file
	return []byte(symlinkTarget), fs.OK
}

func (n *FSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Msg("Readdir called")

	dirEntries := n.filesystem.storage.Metadata().ListDirectory(n.clipNode.Path)
	return fs.NewListDirStream(dirEntries), fs.OK
}

func (n *FSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Str("name", name).Uint32("flags", flags).Uint32("mode", mode).Msg("Create called")
	return nil, nil, 0, syscall.EROFS
}

func (n *FSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	log.Debug().Str("path", n.clipNode.Path).Str("name", name).Uint32("mode", mode).Msg("Mkdir called")
	return nil, syscall.EROFS
}

func (n *FSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	log.Debug().Str("path", n.clipNode.Path).Str("name", name).Msg("Rmdir called")
	return syscall.EROFS
}

func (n *FSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	log.Debug().Str("path", n.clipNode.Path).Str("name", name).Msg("Unlink called")
	return syscall.EROFS
}

func (n *FSNode) Rename(ctx context.Context, oldName string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	log.Debug().Str("path", n.clipNode.Path).Str("old_name", oldName).Str("new_name", newName).Uint32("flags", flags).Msg("Rename called")
	return syscall.EROFS
}
