package clip

import (
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/assert"
)

func TestFuseAttrTimeSanitizesUnsetAndPreEpochTimes(t *testing.T) {
	sec, nsec := fuseAttrTime(time.Time{})
	assert.Equal(t, uint64(0), sec)
	assert.Equal(t, uint32(0), nsec)

	sec, nsec = fuseAttrTime(time.Unix(-1, 123))
	assert.Equal(t, uint64(0), sec)
	assert.Equal(t, uint32(0), nsec)

	sec, nsec = fuseAttrTimespec(10, maxFuseAttrTimeNsec+1)
	assert.Equal(t, uint64(0), sec)
	assert.Equal(t, uint32(0), nsec)

	sec, nsec = fuseAttrTime(time.Unix(42, 123))
	assert.Equal(t, uint64(42), sec)
	assert.Equal(t, uint32(123), nsec)
}

func TestSanitizeFuseAttrTimesHandlesCachedInvalidTimes(t *testing.T) {
	attr := sanitizeFuseAttrTimes(fuse.Attr{
		Atime:     maxFuseAttrTimeSec + 1,
		Atimensec: 1,
		Mtime:     42,
		Mtimensec: 123,
		Ctime:     7,
		Ctimensec: uint32(maxFuseAttrTimeNsec + 1),
	})

	assert.Equal(t, uint64(0), attr.Atime)
	assert.Equal(t, uint32(0), attr.Atimensec)
	assert.Equal(t, uint64(42), attr.Mtime)
	assert.Equal(t, uint32(123), attr.Mtimensec)
	assert.Equal(t, uint64(0), attr.Ctime)
	assert.Equal(t, uint32(0), attr.Ctimensec)
}
