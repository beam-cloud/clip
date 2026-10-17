package clip

import "github.com/hanwen/go-fuse/v2/fuse"

// Immutable directory cookies are stable array offsets. Once Linux learns
// ENOSYS it omits OPENDIR/RELEASEDIR; retain the bridge's directory and PLUS
// semantics by using a short-lived handle only within each read callback.
func (f *immutableNodeFS) OpenDir(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	if f.noOpenDir {
		return fuse.ENOSYS
	}
	return f.RawFileSystem.OpenDir(cancel, in, out)
}

func (f *immutableNodeFS) readDirectory(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList, plus bool) fuse.Status {
	read := f.RawFileSystem.ReadDir
	if plus {
		read = f.RawFileSystem.ReadDirPlus
	}
	if !f.noOpenDir || in.Fh != 0 {
		return read(cancel, in, out)
	}
	var opened fuse.OpenOut
	open := fuse.OpenIn{InHeader: in.InHeader, Flags: in.Flags}
	if status := f.RawFileSystem.OpenDir(cancel, &open, &opened); status != fuse.OK {
		return status
	}
	defer f.RawFileSystem.ReleaseDir(&fuse.ReleaseIn{InHeader: in.InHeader, Fh: opened.Fh, Flags: in.Flags})
	request := *in
	request.Fh = opened.Fh
	return read(cancel, &request, out)
}

func (f *immutableNodeFS) ReadDir(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return f.readDirectory(cancel, in, out, false)
}

func (f *immutableNodeFS) ReadDirPlus(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	return f.readDirectory(cancel, in, out, true)
}
