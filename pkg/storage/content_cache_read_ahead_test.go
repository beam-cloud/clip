package storage

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// countingContentCache records every window fetch offset.
type countingContentCache struct {
	mu      sync.Mutex
	data    []byte
	offsets []int64
}

func (c *countingContentCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	c.mu.Lock()
	c.offsets = append(c.offsets, offset)
	c.mu.Unlock()
	return c.data[offset : offset+length], nil
}

func (c *countingContentCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	return "", nil
}

// Small sequential reads (2000 x 100 KiB files laid out back to back in a
// layer) straddle window boundaries constantly. Each window must be fetched
// exactly once and the reader must be recognised as sequential so prefetch
// depth ramps up instead of resetting at every straddling read.
func TestContentCacheReadAheadStraddlingReadsFetchEachWindowOnce(t *testing.T) {
	const fileSize = 100 << 10
	size := int64(2000 * fileSize)
	cache := &countingContentCache{data: make([]byte, size)}
	for i := range cache.data {
		cache.data[i] = byte(i % 251)
	}
	ra := NewContentCacheReadAhead(cache, ContentCacheReadAheadOptions{})
	dest := make([]byte, fileSize)
	for i := int64(0); i < 2000; i++ {
		off := i * fileSize
		n, err := ra.Read("h", off, dest, struct{ RoutingKey string }{}, size)
		if err != nil || n != fileSize {
			t.Fatalf("read %d: n=%d err=%v", i, n, err)
		}
		if !bytes.Equal(dest, cache.data[off:off+fileSize]) {
			t.Fatalf("read %d returned wrong bytes", i)
		}
	}
	ra.WaitPrefetches()

	counts := map[int64]int{}
	for _, o := range cache.offsets {
		if o%ra.windowBytes != 0 {
			t.Fatalf("unaligned window fetch at offset %d", o)
		}
		counts[o]++
	}
	windows := int((size + ra.windowBytes - 1) / ra.windowBytes)
	if len(counts) != windows {
		t.Fatalf("fetched %d distinct windows, want %d", len(counts), windows)
	}
	for off, c := range counts {
		if c != 1 {
			t.Fatalf("window at %d fetched %d times", off, c)
		}
	}
}

func TestWarmPacerOnlyThrottlesWhileForegroundReadsAreRecent(t *testing.T) {
	s := &OCIClipStorage{}
	pace := s.warmPacer()

	// No foreground reads: 64 MiB of chunks pass through with no sleeping.
	start := time.Now()
	for i := 0; i < 16; i++ {
		pace(4 << 20)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("idle warm was paced: %s", el)
	}

	// A reader is active: 32 MiB should take about half a second at the
	// contended rate.
	s.lastForegroundReadNanos.Store(time.Now().UnixNano())
	start = time.Now()
	for i := 0; i < 8; i++ {
		s.lastForegroundReadNanos.Store(time.Now().UnixNano())
		pace(4 << 20)
	}
	el := time.Since(start)
	want := time.Duration(float64(32<<20) / float64(warmPacerContendedBytesPerSec) * float64(time.Second))
	if el < want*8/10 || el > want*2 {
		t.Fatalf("contended warm ran for %s, want about %s", el, want)
	}

	// Reader went quiet: full speed again.
	time.Sleep(warmPacerActiveWindow + 20*time.Millisecond)
	start = time.Now()
	for i := 0; i < 16; i++ {
		pace(4 << 20)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("warm stayed paced after reader went idle: %s", el)
	}
}
