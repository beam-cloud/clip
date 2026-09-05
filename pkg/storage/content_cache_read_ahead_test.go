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
	pace := s.warmPacer("layer-a")

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

func TestWarmPacerBudgetIsSharedAcrossConcurrentRestores(t *testing.T) {
	s := &OCIClipStorage{}
	s.lastForegroundReadNanos.Store(time.Now().UnixNano())

	// Two restores writing at once should together take about as long as one
	// writing the same total: 32 MiB across both at the contended rate.
	start := time.Now()
	var wg sync.WaitGroup
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pace := s.warmPacer("layer-a")
			for i := 0; i < 4; i++ {
				s.lastForegroundReadNanos.Store(time.Now().UnixNano())
				pace(4 << 20)
			}
		}()
	}
	wg.Wait()
	el := time.Since(start)
	want := time.Duration(float64(32<<20) / float64(warmPacerContendedBytesPerSec) * float64(time.Second))
	if el < want*8/10 || el > want*2 {
		t.Fatalf("two contended restores ran for %s, want about %s (shared budget)", el, want)
	}
}

func TestWarmPacerYieldsToForegroundLayerWaiters(t *testing.T) {
	s := &OCIClipStorage{}
	s.lastForegroundReadNanos.Store(time.Now().UnixNano())
	s.foregroundLayerWaiters.Add(1)
	pace := s.warmPacer("layer-a")
	start := time.Now()
	for i := 0; i < 16; i++ {
		s.lastForegroundReadNanos.Store(time.Now().UnixNano())
		pace(4 << 20)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("restore was paced while a foreground caller waited on a layer: %s", el)
	}
}

func TestWarmPacerDoesNotThrottleALayerBeingRead(t *testing.T) {
	s := &OCIClipStorage{}
	now := time.Now().UnixNano()
	s.lastForegroundReadNanos.Store(now)
	s.lastReadByLayer.Store("hot", now)

	// The layer the container is reading restores at full speed...
	pace := s.warmPacer("hot")
	start := time.Now()
	for i := 0; i < 16; i++ {
		s.lastReadByLayer.Store("hot", time.Now().UnixNano())
		s.lastForegroundReadNanos.Store(time.Now().UnixNano())
		pace(4 << 20)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("restore of a layer being read was paced: %s", el)
	}

	// ...while a layer nobody is reading is paced on the same mount.
	cold := s.warmPacer("cold")
	start = time.Now()
	for i := 0; i < 4; i++ {
		s.lastForegroundReadNanos.Store(time.Now().UnixNano())
		cold(4 << 20)
	}
	want := time.Duration(float64(16<<20) / float64(warmPacerContendedBytesPerSec) * float64(time.Second))
	if el := time.Since(start); el < want*8/10 {
		t.Fatalf("restore of an unread layer was not paced: %s, want about %s", el, want)
	}
}
