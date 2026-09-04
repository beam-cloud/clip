package clip

import (
	"testing"
	"time"
)

func TestImmutableFilesystemOptionsCacheMetadata(t *testing.T) {
	options := immutableFilesystemOptions()
	timeouts := map[string]*time.Duration{
		"attribute": options.AttrTimeout,
		"entry":     options.EntryTimeout,
		"negative":  options.NegativeTimeout,
	}
	for name, timeout := range timeouts {
		if timeout == nil || *timeout != immutableMetadataCacheTimeout {
			t.Fatalf("%s timeout = %v, want %s", name, timeout, immutableMetadataCacheTimeout)
		}
	}
}

func TestSpliceSafeMaxWriteFor(t *testing.T) {
	const page = 4096
	for _, c := range []struct{ pipeMax, want int }{
		{1 << 20, (1 << 20) - 2*page},
		{4 << 20, (4 << 20) - 2*page},
		{2 * page, 0},
		{0, 0},
	} {
		got := spliceSafeMaxWriteFor(c.pipeMax, page)
		if got != c.want {
			t.Fatalf("spliceSafeMaxWriteFor(%d) = %d, want %d", c.pipeMax, got, c.want)
		}
		if got > 0 && 16+got+page > c.pipeMax {
			t.Fatalf("pipeMax %d: %d leaves no room for header and extra page", c.pipeMax, got)
		}
	}
}
