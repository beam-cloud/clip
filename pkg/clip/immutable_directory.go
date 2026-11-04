package clip

import (
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// Linux FUSE_NO_OPENDIR_SUPPORT (fuse protocol 7.29). Request it through
// ExtraCapabilities; older kernels keep the original handle-based path.
const noOpendirSupport = uint64(1 << 24)

// Image directory entries and their ordering are immutable for the mount's
// lifetime. Most directory opens never enumerate anything. Let capable kernels
// skip OPENDIR/RELEASEDIR, creating a temporary bridge handle only for READDIR.
// Reconstructing the list stream preserves its index-based offset cookies,
// while delegating READDIRPLUS keeps go-fuse's inode and lookup accounting.
type immutableDirectoryFS struct {
	fuse.RawFileSystem
	stateless  bool
	filesystem *ClipFileSystem
}

func (f *immutableDirectoryFS) Init(server *fuse.Server) {
	flags := server.KernelSettings().Flags64()
	f.stateless = flags&noOpendirSupport != 0
	f.filesystem.statelessOpens = flags&fuse.CAP_NO_OPEN_SUPPORT != 0
	f.RawFileSystem.Init(server)
}

func (f *immutableDirectoryFS) OpenDir(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	if f.stateless {
		return fuse.ENOSYS
	}
	return f.RawFileSystem.OpenDir(cancel, in, out)
}

func (f *immutableDirectoryFS) ReadDir(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return f.readDirectory(cancel, in, out, false)
}

func (f *immutableDirectoryFS) ReadDirPlus(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return f.readDirectory(cancel, in, out, true)
}

func (f *immutableDirectoryFS) readDirectory(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList, plus bool) fuse.Status {
	if f.stateless {
		var opened fuse.OpenOut
		if status := f.RawFileSystem.OpenDir(cancel, &fuse.OpenIn{InHeader: in.InHeader, Flags: syscall.O_RDONLY | syscall.O_DIRECTORY}, &opened); status != fuse.OK {
			return status
		}
		defer f.RawFileSystem.ReleaseDir(&fuse.ReleaseIn{InHeader: in.InHeader, Fh: opened.Fh})
		copy := *in
		copy.Fh = opened.Fh
		in = &copy
	}
	if plus {
		return f.RawFileSystem.ReadDirPlus(cancel, in, out)
	}
	return f.RawFileSystem.ReadDir(cancel, in, out)
}

func (f *immutableDirectoryFS) FsyncDir(cancel <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	if f.stateless {
		return fuse.OK // The image is read-only and has no dirty directory state.
	}
	return f.RawFileSystem.FsyncDir(cancel, in)
}
