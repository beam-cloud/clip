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
