package clip

import (
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

const maxFuseAttrTimeNsec int64 = 999999999
const maxFuseAttrTimeSec uint64 = 1<<63 - 1

func fuseAttrTime(t time.Time) (uint64, uint32) {
	if t.IsZero() {
		return 0, 0
	}
	return fuseAttrTimespec(t.Unix(), int64(t.Nanosecond()))
}

func fuseAttrTimespec(sec int64, nsec int64) (uint64, uint32) {
	if sec < 0 || nsec < 0 || nsec > maxFuseAttrTimeNsec {
		return 0, 0
	}
	return uint64(sec), uint32(nsec)
}

func sanitizeFuseAttrTimes(attr fuse.Attr) fuse.Attr {
	attr.Atime, attr.Atimensec = sanitizeFuseAttrTime(attr.Atime, attr.Atimensec)
	attr.Mtime, attr.Mtimensec = sanitizeFuseAttrTime(attr.Mtime, attr.Mtimensec)
	attr.Ctime, attr.Ctimensec = sanitizeFuseAttrTime(attr.Ctime, attr.Ctimensec)
	return attr
}

func sanitizeFuseAttrTime(sec uint64, nsec uint32) (uint64, uint32) {
	if sec > maxFuseAttrTimeSec || nsec > uint32(maxFuseAttrTimeNsec) {
		return 0, 0
	}
	return sec, nsec
}
